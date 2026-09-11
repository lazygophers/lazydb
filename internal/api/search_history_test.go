// #23 搜索 + 历史 + 审计（HTTP 缝，含 MCP 路径留痕见 mcpserver 测试）。
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/audit"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/history"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

// newTrackedServer 带 history + audit 的后端。
func newTrackedServer(t *testing.T, dir string) (*httptest.Server, string) {
	t.Helper()
	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	hist, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hist.Close() })
	aud, err := audit.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aud.Close() })
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) {
		return &sqlsrc.SQLite{}, nil
	})
	ts := httptest.NewServer(api.New(m, store, token, hist, aud))
	t.Cleanup(ts.Close)

	dsn := filepath.Join(dir, "t.db")
	var src sqlsrc.SQLite
	if err := src.Open(t.Context(), source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, stmt := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer_name TEXT NOT NULL)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT)`,
	} {
		if _, err := src.Exec(t.Context(), stmt, source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(map[string]any{"name": "t", "config": map[string]string{"driver": "sqlite", "dsn": dsn}})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)
	return ts, c.ID
}

func search(t *testing.T, ts *httptest.Server, id, q string) []map[string]any {
	t.Helper()
	res := do(t, ts, "GET",
		fmt.Sprintf("/api/connections/%s/search?q=%s", id, q), nil, http.StatusOK)
	var r struct {
		Matches []map[string]any `json:"matches"`
	}
	json.Unmarshal(res, &r)
	return r.Matches
}

func TestSearchTablesAndColumns(t *testing.T) {
	ts, id := newTrackedServer(t, t.TempDir())

	ms := search(t, ts, id, "orde")
	if len(ms) != 1 || ms[0]["kind"] != "table" || ms[0]["table"] != "main.orders" {
		t.Fatalf("table search = %+v", ms)
	}
	ms = search(t, ts, id, "cust")
	if len(ms) != 1 || ms[0]["kind"] != "column" || ms[0]["table"] != "main.orders" {
		t.Fatalf("column search = %+v", ms)
	}
	ms = search(t, ts, id, "zzz")
	if len(ms) != 0 {
		t.Fatalf("no match = %+v", ms)
	}
}

// 万表即时：首搜建内存索引，二搜纯内存。列名也在万表规模可搜。
func TestSearchTenThousandTablesFast(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "big.db")
	var src sqlsrc.SQLite
	if err := src.Open(t.Context(), source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		if _, err := src.Exec(t.Context(),
			fmt.Sprintf("CREATE TABLE big_%05d (c%d TEXT)", i, i), source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	src.Close()

	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) { return &sqlsrc.SQLite{}, nil })
	ts := httptest.NewServer(api.New(m, store, token, nil, nil))
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{"name": "b", "config": map[string]string{"driver": "sqlite", "dsn": dsn}})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)

	start := time.Now()
	ms := search(t, ts, c.ID, "big_0999")
	build := time.Since(start)
	if len(ms) < 1 {
		t.Fatalf("matches = %+v", ms)
	}
	start = time.Now()
	ms = search(t, ts, c.ID, "c9") // 列名
	warm := time.Since(start)
	if len(ms) < 1 {
		t.Fatalf("column matches = %+v", ms)
	}
	if warm > 300*time.Millisecond {
		t.Fatalf("warm search too slow: %v (build was %v)", warm, build)
	}
	t.Logf("index build: %v, warm search: %v", build, warm)
}

func TestHistoryRecordsExec(t *testing.T) {
	ts, id := newTrackedServer(t, t.TempDir())

	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT * FROM orders"}), http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT nope FROM orders"}), http.StatusBadRequest)

	res := do(t, ts, "GET", "/api/history?q=", nil, http.StatusOK)
	var h struct {
		Items []history.Entry `json:"items"`
	}
	json.Unmarshal(res, &h)
	if len(h.Items) != 2 {
		t.Fatalf("history = %+v", h.Items)
	}
	// 倒序：最新在前
	if !strings.Contains(h.Items[0].SQL, "nope") || h.Items[0].OK {
		t.Fatalf("newest = %+v", h.Items[0])
	}
	if !h.Items[1].OK || h.Items[1].SQL != "SELECT * FROM orders" {
		t.Fatalf("older entry = %+v", h.Items[1])
	}

	// 关键字搜
	res = do(t, ts, "GET", "/api/history?q=nope", nil, http.StatusOK)
	json.Unmarshal(res, &h)
	if len(h.Items) != 1 {
		t.Fatalf("filtered history = %+v", h.Items)
	}
}

// 断网搜索：缓存里已有结构后，数据源完全不可达，搜索仍出结果（stale-fallback）。
type deadSrc struct{}

func (deadSrc) Open(context.Context, source.Config) error  { return nil }
func (deadSrc) Close() error                                { return errors.New("down") }
func (deadSrc) Ping(context.Context) error                  { return errors.New("down") }
func (deadSrc) Children(context.Context, source.Path) ([]source.Node, error) {
	return nil, errors.New("down")
}
func (deadSrc) Exec(context.Context, string, source.ExecOptions) (source.Result, error) {
	return source.Result{}, errors.New("down")
}

func TestSearchOfflineFromCache(t *testing.T) {
	dir := t.TempDir()
	ts, id := newTrackedServer(t, dir)
	if ms := search(t, ts, id, "orde"); len(ms) != 1 { // 先联网建索引落缓存
		t.Fatalf("warm-up search = %+v", ms)
	}
	ts.Close() // 换成数据源全挂的后端，共享同一缓存目录

	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) { return deadSrc{}, nil })
	ts2 := httptest.NewServer(api.New(m, store, token, nil, nil))
	t.Cleanup(ts2.Close)

	dsn := filepath.Join(dir, "t.db")
	body, _ := json.Marshal(map[string]any{"name": "t", "config": map[string]string{"driver": "sqlite", "dsn": dsn}})
	res := do(t, ts2, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)

	ms := search(t, ts2, c.ID, "orde")
	if len(ms) != 1 || ms[0]["table"] != "main.orders" {
		t.Fatalf("offline search = %+v", ms)
	}
}

// 审计留痕：HTTP 路径每次执行一行 JSON，成败都记。
func TestAuditLogUIPath(t *testing.T) {
	dir := t.TempDir()
	ts, id := newTrackedServer(t, dir)

	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT 1"}), http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT bad"}), http.StatusBadRequest)

	b, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d: %s", len(lines), b)
	}
	var first audit.Entry
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first.Source != "ui" || first.Conn != id || first.SQL != "SELECT 1" || !first.OK {
		t.Fatalf("first audit = %+v", first)
	}
	var second audit.Entry
	json.Unmarshal([]byte(lines[1]), &second)
	if second.OK || second.Err == "" {
		t.Fatalf("second audit = %+v", second)
	}
}
