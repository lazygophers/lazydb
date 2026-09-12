// #29 设置端点（HTTP 缝）：GET 默认 → PUT 回读 → 文件 0600 + 新进程重开持久化。
package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/settings"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

// newSettingsServer：同 newServerAt，但路由带设置存储。
func newSettingsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	st, err := settings.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) {
		return &sqlsrc.SQLite{}, nil
	})
	ts := httptest.NewServer(api.NewWithSettings(m, store, token, nil, nil, st))
	t.Cleanup(ts.Close)
	return ts, dir
}

func TestSettingsGetDefault(t *testing.T) {
	ts, _ := newSettingsServer(t)
	var got map[string]any
	if err := json.Unmarshal(do(t, ts, "GET", "/api/settings", nil, 200), &got); err != nil {
		t.Fatal(err)
	}
	if got["keep_core_on_close"] != true {
		t.Fatalf("GET 默认 keep_core_on_close 应 true：%v", got)
	}
}

func TestSettingsPutRoundtrip(t *testing.T) {
	ts, dir := newSettingsServer(t)
	var got map[string]any
	if err := json.Unmarshal(do(t, ts, "PUT", "/api/settings",
		[]byte(`{"keep_core_on_close":false}`), 200), &got); err != nil {
		t.Fatal(err)
	}
	if got["keep_core_on_close"] != false {
		t.Fatalf("PUT 后应回读 false：%v", got)
	}

	fi, err := os.Stat(filepath.Join(dir, ".lazydb", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("settings.json 应 0600：%v", fi.Mode().Perm())
	}

	// 新进程（同 home）重开读到持久化值
	st2, err := settings.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Get().KeepCoreOnClose {
		t.Error("持久化失败：新 Store 仍读到 true")
	}
}

func TestSettingsBadPut(t *testing.T) {
	ts, _ := newSettingsServer(t)
	do(t, ts, "PUT", "/api/settings", []byte(`{oops`), 400)
}

// driver_index_url（#33）：改 URL 回读，留空恢复默认。
func TestSettingsDriverIndexURLRoundtrip(t *testing.T) {
	ts, _ := newSettingsServer(t)
	body, _ := json.Marshal(map[string]any{"keep_core_on_close": true, "driver_index_url": "http://mirror.local/index.json"})
	do(t, ts, "PUT", "/api/settings", body, http.StatusOK)
	var s settings.Settings
	if err := json.Unmarshal(do(t, ts, "GET", "/api/settings", nil, http.StatusOK), &s); err != nil {
		t.Fatal(err)
	}
	if s.DriverIndexURL != "http://mirror.local/index.json" {
		t.Fatalf("driver_index_url = %q", s.DriverIndexURL)
	}
	// 留空 = 恢复默认
	body, _ = json.Marshal(map[string]any{"keep_core_on_close": true})
	do(t, ts, "PUT", "/api/settings", body, http.StatusOK)
	if err := json.Unmarshal(do(t, ts, "GET", "/api/settings", nil, http.StatusOK), &s); err != nil {
		t.Fatal(err)
	}
	if s.DriverIndexURL != "" {
		t.Fatalf("cleared driver_index_url = %q, want empty (= default)", s.DriverIndexURL)
	}
}
