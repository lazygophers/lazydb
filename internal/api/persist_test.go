// #25 凭据不明文（HTTP 缝）：连接落盘恢复全链路 + 磁盘文件无明文密码扫描。
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/connstore"
	"github.com/lazygophers/lazydb/internal/secrets"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

// fakeSecrets 内存钥匙串（secrets.Store 的测试实现）。
type fakeSecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *fakeSecrets) Set(id, dsn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = dsn
	return nil
}

func (f *fakeSecrets) Get(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dsn, ok := f.m[id]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return dsn, nil
}

func (f *fakeSecrets) Delete(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, id)
	return nil
}

func (f *fakeSecrets) Keys() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.m {
		ids = append(ids, id)
	}
	return ids, nil
}

// newPersistServer 带 connstore 的后端（与 main 同构）。sec 传 nil 则新建。
func newPersistServer(t *testing.T, dir string, sec *fakeSecrets) (*httptest.Server, *fakeSecrets) {
	t.Helper()
	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) { return &sqlsrc.SQLite{}, nil })
	if sec == nil {
		sec = &fakeSecrets{m: map[string]string{}}
	}
	if err := connstore.Wire(m, sec, dir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.New(m, store, token, nil, nil))
	t.Cleanup(ts.Close)
	return ts, sec
}

// 建连接（DSN 带密码，虽然 sqlite 不认——检查落盘行为的不是连通性）。
func addConn(t *testing.T, ts *httptest.Server, dir, name string) string {
	t.Helper()
	dsn := filepath.Join(dir, "t.db")
	if name == "prod" {
		dsn = "root:S3cretPass@tcp(10.0.0.1:3306)/prod?parseTime=true"
	}
	body, _ := json.Marshal(map[string]any{"name": name, "config": map[string]string{"driver": "sqlite", "dsn": dsn}})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)
	return c.ID
}

// 建连接 + 跑查询落缓存/历史后：connections.json 里无密码；钥匙串里有 DSN；
// 删连接后钥匙串项也清掉。
func TestCredentialsNotOnDisk(t *testing.T) {
	dir := t.TempDir()
	ts, sec := newPersistServer(t, dir, nil)

	id := addConn(t, ts, dir, "prod")
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": "SELECT S3cretPass"}), http.StatusBadRequest) // 失败也写历史/审计

	b, err := os.ReadFile(filepath.Join(dir, ".lazydb", "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "S3cretPass") {
		t.Fatalf("connections.json 泄明文: %s", b)
	}
	dsn, err := sec.Get(id)
	if err != nil || !strings.Contains(dsn, "S3cretPass") {
		t.Fatalf("keyring dsn = %q err=%v", dsn, err)
	}

	// 全目录扫描：任何落盘文件都不得含密码（cache/history/audit/connections.json）
	_ = filepath.Walk(dir, func(path string, _ os.FileInfo, err error) error {
		if err != nil || path == filepath.Join(dir, "t.db") {
			return nil // t.db 是数据源本体（本测试没真写进密码）
		}
		f, e := os.Open(path)
		if e != nil {
			return nil
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if bytes.Contains(data, []byte("S3cretPass")) {
			t.Errorf("明文密码出现在 %s", path)
		}
		return nil
	})

	// 删除连接：钥匙串项随之清掉
	do(t, ts, "DELETE", fmt.Sprintf("/api/connections/%s", id), nil, http.StatusNoContent)
	if keys, _ := sec.Keys(); len(keys) != 0 {
		t.Fatalf("orphan keyring keys = %v", keys)
	}
}

// 重启恢复：同 home 新后端起来，连接列表原样回来。
func TestConnectionsRestoreAfterRestart(t *testing.T) {
	dir := t.TempDir()
	ts, sec := newPersistServer(t, dir, nil)
	addConn(t, ts, dir, "local")
	addConn(t, ts, dir, "prod")
	ts.Close()

	ts2, _ := newPersistServer(t, dir, sec)
	res := do(t, ts2, "GET", "/api/connections", nil, http.StatusOK)
	var list []map[string]any
	json.Unmarshal(res, &list)
	if len(list) != 2 || list[0]["name"] != "local" || list[1]["name"] != "prod" {
		t.Fatalf("restored = %+v", list)
	}
	// 恢复回来的连接可用（sqlite 链路）
	do(t, ts2, "POST", fmt.Sprintf("/api/connections/%s/exec", list[0]["id"]),
		mustJSON(t, map[string]string{"sql": "SELECT 1"}), http.StatusOK)
}
