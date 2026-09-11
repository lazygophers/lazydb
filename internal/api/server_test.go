// 主测试缝：HTTP API（spec #15 测试决策）。SQLite 当测试数据源。
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

const token = "test-token"

func newServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) {
		return &sqlsrc.SQLite{}, nil
	})
	ts := httptest.NewServer(api.New(m, token))
	t.Cleanup(ts.Close)
	return ts, dsn
}

// createConn 建连接并返回 id。
func createConn(t *testing.T, ts *httptest.Server, dsn string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name":   "t",
		"config": map[string]string{"driver": "sqlite", "dsn": dsn},
	})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	if err := json.Unmarshal(res, &c); err != nil {
		t.Fatalf("decode conn: %v", err)
	}
	return c.ID
}

func do(t *testing.T, ts *httptest.Server, method, path string, body []byte, wantStatus int) []byte {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(res.Body)
	if res.StatusCode != wantStatus {
		t.Fatalf("%s %s: status %d want %d, body: %s",
			method, path, res.StatusCode, wantStatus, out.String())
	}
	return out.Bytes()
}

// 全链路：建连接 → Ping → Children 列树 → 能力接口 → Exec。
func TestFullChainOverHTTP(t *testing.T) {
	ts, dsn := newServer(t)
	id := createConn(t, ts, dsn)

	// 建表 + 写数据（经 HTTP 缝的 Exec）
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `CREATE TABLE users (
			id INTEGER PRIMARY KEY, name TEXT NOT NULL, email TEXT)`}), http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `INSERT INTO users (name, email) VALUES
			('alice','a@x.com'), ('bob','b@x.com')`}), http.StatusOK)

	// Ping
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/ping", id), nil, http.StatusOK)

	// Children：根 → main
	var root struct {
		Nodes []source.Node `json:"nodes"`
	}
	json.Unmarshal(do(t, ts, "GET", "/api/connections/"+id+"/children", nil, http.StatusOK), &root)
	if len(root.Nodes) != 1 || root.Nodes[0].Name != "main" || root.Nodes[0].Kind != "database" {
		t.Fatalf("root children = %+v", root.Nodes)
	}

	// Children：main → users 表
	var tables struct {
		Nodes []source.Node `json:"nodes"`
	}
	json.Unmarshal(do(t, ts, "GET", "/api/connections/"+id+"/children?path=main", nil, http.StatusOK), &tables)
	if len(tables.Nodes) != 1 || tables.Nodes[0].Name != "users" || tables.Nodes[0].Kind != "table" {
		t.Fatalf("tables = %+v", tables.Nodes)
	}

	// 能力探测：capabilities 全 true
	var caps map[string]bool
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/capabilities", id), nil, http.StatusOK), &caps)
	if !caps["columnLister"] || !caps["indexLister"] || !caps["ddlShower"] {
		t.Fatalf("capabilities = %+v", caps)
	}

	// Columns
	var cols struct {
		Columns []source.Column `json:"columns"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/columns?path=main&path=users", id), nil, http.StatusOK), &cols)
	if len(cols.Columns) != 3 {
		t.Fatalf("columns = %+v", cols.Columns)
	}
	if cols.Columns[1].Name != "name" || cols.Columns[1].Nullable {
		t.Fatalf("column name = %+v", cols.Columns[1])
	}

	// DDL
	var ddl struct{ DDL string }
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/ddl?path=main&path=users", id), nil, http.StatusOK), &ddl)
	if ddl.DDL == "" || !bytes.Contains([]byte(ddl.DDL), []byte("CREATE TABLE")) {
		t.Fatalf("ddl = %q", ddl.DDL)
	}

	// Exec 查询 + MaxRows 截断
	var res source.Result
	json.Unmarshal(do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT id, name FROM users ORDER BY id", "max_rows": 1}), http.StatusOK), &res)
	if len(res.Rows) != 1 || !res.Truncated || res.Rows[0][1] != "alice" {
		t.Fatalf("exec res = %+v", res)
	}
}

func TestIndexes(t *testing.T) {
	ts, dsn := newServer(t)
	id := createConn(t, ts, dsn)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `CREATE TABLE t (a TEXT, b TEXT);
			CREATE UNIQUE INDEX idx_ab ON t(a, b)`}), http.StatusOK)

	var out struct {
		Indexes []source.Index `json:"indexes"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/indexes?path=main&path=t", id), nil, http.StatusOK), &out)
	if len(out.Indexes) != 1 {
		t.Fatalf("indexes = %+v", out.Indexes)
	}
	ix := out.Indexes[0]
	if ix.Name != "idx_ab" || !ix.Unique || fmt.Sprint(ix.Columns) != "[a b]" {
		t.Fatalf("index = %+v", ix)
	}
}

// 非法 SQL → 结构化错误（不 panic、JSON 带 code）。
func TestInvalidSQLStructuredError(t *testing.T) {
	ts, dsn := newServer(t)
	id := createConn(t, ts, dsn)

	res := do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": "SELCT 1"}), http.StatusBadRequest)
	var e struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(res, &e); err != nil {
		t.Fatalf("error not JSON: %v, body=%s", err, res)
	}
	if e.Error.Code != "sql_error" || e.Error.Message == "" {
		t.Fatalf("error = %+v", e.Error)
	}
}

func TestAuthAndNotFound(t *testing.T) {
	ts, dsn := newServer(t)
	id := createConn(t, ts, dsn)

	// 错 token → 401 结构化错误
	req, _ := http.NewRequest("GET", ts.URL+"/api/connections", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", res.StatusCode)
	}

	// 不存在的连接 → 404
	do(t, ts, "GET", "/api/connections/999/children", nil, http.StatusNotFound)

	// 未知驱动 → 400 open_failed
	do(t, ts, "POST", "/api/connections",
		mustJSON(t, map[string]any{"name": "x", "config": map[string]string{"driver": "nope"}}),
		http.StatusBadRequest)

	// 删除后 404
	do(t, ts, "DELETE", "/api/connections/"+id, nil, http.StatusNoContent)
	do(t, ts, "GET", "/api/connections/"+id+"/children", nil, http.StatusNotFound)
}

// DSN 落盘可复用：重启「后端」（新 Manager）后同一文件可继续读。
func TestReopenDBFile(t *testing.T) {
	ts, dsn := newServer(t)
	id := createConn(t, ts, dsn)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": "CREATE TABLE keep (x INT)"}), http.StatusOK)

	ts2, _ := newServer(t) // 新 Manager = 模拟重启
	id2 := createConn(t, ts2, dsn)
	var tables struct {
		Nodes []source.Node `json:"nodes"`
	}
	json.Unmarshal(do(t, ts2, "GET", fmt.Sprintf("/api/connections/%s/children?path=main", id2), nil, http.StatusOK), &tables)
	if len(tables.Nodes) != 1 || tables.Nodes[0].Name != "keep" {
		t.Fatalf("after reopen tables = %+v", tables.Nodes)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
