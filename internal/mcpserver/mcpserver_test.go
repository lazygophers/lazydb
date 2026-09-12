// MCP stdio 缝测试（辅缝，spec #15）。子进程 = 本测试二进制 re-exec
// （LAZYDB_MCP_CHILD=1 时 TestMain 直接跑 mcpserver，不走测试），
// 经 SDK CommandTransport 真 stdio 握手、列工具、调工具。
package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/mcpserver"
	"github.com/lazygophers/lazydb/internal/secrets"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSecStore：文件版假钥匙串（每 id 一个文件），只在测试注入。
// 生产语义由 RunWithSecrets(sec=nil) 走「无钥匙串」分支覆盖。
type fakeSecStore struct{ dir string }

func (f fakeSecStore) Set(id, dsn string) error {
	return os.WriteFile(filepath.Join(f.dir, id), []byte(dsn), 0o600)
}

func (f fakeSecStore) Get(id string) (string, error) {
	b, err := os.ReadFile(filepath.Join(f.dir, id))
	return string(b), err
}

func (f fakeSecStore) Delete(id string) error { return os.Remove(filepath.Join(f.dir, id)) }

func (f fakeSecStore) Keys() ([]string, error) {
	es, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out, nil
}

func TestMain(m *testing.M) {
	if os.Getenv("LAZYDB_MCP_CHILD") == "1" {
		home := os.Getenv("LAZYDB_MCP_HOME")
		allowWrite := os.Getenv("LAZYDB_MCP_ALLOW_WRITE") == "true"
		// 测试不碰真钥匙串：默认无钥匙串模式；设 FAKE_SEC_DIR 时注入文件假钥匙串
		var sec secrets.Store
		if d := os.Getenv("LAZYDB_MCP_FAKE_SEC_DIR"); d != "" {
			sec = fakeSecStore{d}
		}
		if err := mcpserver.RunWithSecrets(context.Background(), home, allowWrite, sec); err != nil {
			fmt.Fprintln(os.Stderr, "mcp child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newClient 拉起子进程 MCP server 并握手。
func newClient(t *testing.T, home string, allowWrite bool) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"LAZYDB_MCP_CHILD=1",
		"LAZYDB_MCP_HOME="+home,
		fmt.Sprintf("LAZYDB_MCP_ALLOW_WRITE=%t", allowWrite),
	)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lazydb-test-client"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect mcp server: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content len = %d", len(res.Content))
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func setupDB(t *testing.T, home string) string {
	t.Helper()
	dsn := filepath.Join(home, "mcp.db")
	var src sqlsrc.SQLite
	if err := src.Open(context.Background(), source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, stmt := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT NOT NULL)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT, orders_id INTEGER REFERENCES orders(id) ON DELETE CASCADE)`,
		`CREATE UNIQUE INDEX idx_label ON items(label)`,
		`INSERT INTO orders (customer) VALUES ('alice'), ('bob')`,
	} {
		if _, err := src.Exec(context.Background(), stmt, source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

func connect(t *testing.T, cs *mcp.ClientSession, dsn string) string {
	t.Helper()
	res := call(t, cs, "connect", map[string]any{
		"name": "t", "driver": "sqlite", "dsn": dsn,
	})
	id, ok := strings.CutPrefix(textOf(t, res), "conn_id: ")
	if !ok {
		t.Fatalf("connect result = %q", textOf(t, res))
	}
	return strings.TrimSpace(id)
}

func TestMCPHandshakeAndListTools(t *testing.T) {
	home := t.TempDir()
	cs := newClient(t, home, false)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"connect": true, "list_connections": true, "connect_saved": true, "list_databases": true,
		"list_tables": true, "list_columns": true, "get_ddl": true, "search_schema": true, "run_query": true}
	got := map[string]bool{}
	for _, tl := range tools.Tools {
		got[tl.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("tool %s missing; got %v", name, got)
		}
	}
}

func TestMCPAllToolsReadOnlyHappyPath(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	res := call(t, cs, "list_databases", map[string]any{"conn": connID})
	if !strings.Contains(textOf(t, res), "main") {
		t.Fatalf("list_databases = %q", textOf(t, res))
	}
	res = call(t, cs, "list_tables", map[string]any{"conn": connID, "database": "main"})
	if !strings.Contains(textOf(t, res), "orders") || !strings.Contains(textOf(t, res), "items") {
		t.Fatalf("list_tables = %q", textOf(t, res))
	}
	res = call(t, cs, "list_columns", map[string]any{"conn": connID, "database": "main", "table": "orders"})
	if !strings.Contains(textOf(t, res), "customer") || !strings.Contains(textOf(t, res), "NOT NULL") {
		t.Fatalf("list_columns = %q", textOf(t, res))
	}
	res = call(t, cs, "get_ddl", map[string]any{"conn": connID, "database": "main", "object": "items"})
	if !strings.Contains(textOf(t, res), "CREATE TABLE") {
		t.Fatalf("get_ddl = %q", textOf(t, res))
	}
	res = call(t, cs, "search_schema", map[string]any{"conn": connID, "query": "orde"})
	if !strings.Contains(textOf(t, res), "main.orders") {
		t.Fatalf("search_schema = %q", textOf(t, res))
	}
	res = call(t, cs, "run_query", map[string]any{"conn": connID, "sql": "SELECT customer FROM orders ORDER BY id"})
	if !strings.Contains(textOf(t, res), "alice") || !strings.Contains(textOf(t, res), "bob") {
		t.Fatalf("run_query = %q", textOf(t, res))
	}
}

func TestMCPWriteDeniedByDefault(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	for _, sql := range []string{
		"INSERT INTO orders (customer) VALUES ('eve')",
		"UPDATE orders SET customer = 'x'",
		"DELETE FROM orders",
		"DROP TABLE orders",
		"CREATE TABLE evil (x INT)",
		"ALTER TABLE orders ADD COLUMN z INT",
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "run_query", Arguments: map[string]any{"conn": connID, "sql": sql},
		})
		if err == nil && !res.IsError {
			t.Fatalf("write should be denied: %q", sql)
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			msg = textOf(t, res)
		}
		if !strings.Contains(msg, "--allow-write") {
			t.Fatalf("error should mention --allow-write: %q", msg)
		}
	}
}

func TestMCPAllowWrite(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, true)
	connID := connect(t, cs, dsn)

	res := call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "INSERT INTO orders (customer) VALUES ('eve')",
	})
	if res.IsError {
		t.Fatalf("insert under allow-write failed: %s", textOf(t, res))
	}
	res = call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "SELECT COUNT(*) AS n FROM orders",
	})
	if !strings.Contains(textOf(t, res), "3") {
		t.Fatalf("count after insert = %q", textOf(t, res))
	}
}

// MCP 路径执行留审计痕（#23）：run_query 后 audit.log 出现 source=mcp 的行。
func TestMCPAuditTrail(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "SELECT COUNT(*) AS n FROM orders",
	})

	b, err := os.ReadFile(filepath.Join(home, "audit.log"))
	if err != nil {
		t.Fatalf("read audit.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	found := false
	for _, line := range lines {
		var e struct {
			Source string `json:"source"`
			SQL    string `json:"sql"`
			OK     bool   `json:"ok"`
			Conn   string `json:"conn"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("bad audit line %q: %v", line, err)
		}
		if e.Source == "mcp" && strings.Contains(e.SQL, "COUNT") && e.OK && e.Conn == connID {
			found = true
		}
	}
	if !found {
		t.Fatalf("no mcp audit entry in %d lines", len(lines))
	}
}

// mcp-server 与「后端」并发读写 cache.db 无损坏：MCP 会话跑查询的同时// 本进程另开 Store 写，结束后 quick_check = ok，且双方数据都可见。
func TestMCPAndBackendShareCache(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	backend, err := cache.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			_ = backend.Save(context.Background(), "backend-conn", []string{"db"},
				cache.KindChildren, []map[string]string{{"name": "x", "kind": "table"}},
				time.Now())
		}
	}()

	res := call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "SELECT COUNT(*) AS n FROM orders",
	})
	if !strings.Contains(textOf(t, res), "2") {
		t.Fatalf("count during concurrent writes = %q", textOf(t, res))
	}
	<-done

	chk, err := backend.QuickCheck(context.Background())
	if err != nil || chk != "ok" {
		t.Fatalf("quick_check = %q err=%v", chk, err)
	}
}

// 白名单拒绝的错误含被拒语句首词（#30 第一道闸门，不往返数据库）。
func TestMCPWriteDeniedMentionsVerb(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "run_query",
		Arguments: map[string]any{"conn": connID, "sql": "delete from orders"},
	})
	msg := textOf(t, res)
	if err != nil {
		msg = err.Error()
	}
	if !strings.Contains(msg, "delete") || !strings.Contains(msg, "--allow-write") {
		t.Fatalf("error should mention verb and --allow-write: %q", msg)
	}
}

// 只读模式：白名单放行的 CTE 藏写（WITH … DELETE）也被事务回滚拦下（#30）。
func TestMCPReadOnlyRollbackCTEWrite(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	res := call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "WITH v AS (SELECT 1) DELETE FROM orders",
	})
	if res.IsError {
		t.Fatalf("cte delete should run (and rollback): %s", textOf(t, res))
	}
	res = call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "SELECT COUNT(*) AS n FROM orders",
	})
	if !strings.Contains(textOf(t, res), "2") {
		t.Fatalf("count after rolled-back cte delete = %q, want 2", textOf(t, res))
	}
}

// 存档连接（#32）：界面存两条后 MCP 能列、能连；凭据不泄漏。
func TestMCPSavedConnections(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	// DSN 带个明显标记，任何工具返回里出现它 = 泄漏
	if !strings.Contains(dsn, "mcp.db") {
		t.Fatalf("dsn = %q", dsn)
	}
	secDir := t.TempDir()
	fake := fakeSecStore{dir: secDir}
	if err := fake.Set("conn-a", dsn); err != nil {
		t.Fatal(err)
	}
	if err := fake.Set("conn-b", dsn); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(home, ".lazydb"), 0o700)
	if err := os.WriteFile(filepath.Join(home, ".lazydb", "connections.json"),
		[]byte(`[{"id":"conn-a","name":"本地A","driver":"sqlite"},{"id":"conn-b","name":"本地B","driver":"sqlite"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"LAZYDB_MCP_CHILD=1", "LAZYDB_MCP_HOME="+home, "LAZYDB_MCP_ALLOW_WRITE=false",
		"LAZYDB_MCP_FAKE_SEC_DIR="+secDir)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "lazydb-test-client"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "list_connections", Arguments: map[string]any{}})
	var out string
	if err != nil {
		out = err.Error()
	} else {
		out = textOf(t, res)
	}
	if !strings.Contains(out, "本地A") || !strings.Contains(out, "本地B") {
		t.Fatalf("list_connections = %q", out)
	}
	if strings.Contains(out, ".db") {
		t.Fatalf("list_connections leaks dsn: %q", out)
	}

	res = call(t, cs, "connect_saved", map[string]any{"name": "本地A"})
	connID, ok := strings.CutPrefix(textOf(t, res), "conn_id: ")
	if !ok {
		t.Fatalf("connect_saved = %q", textOf(t, res))
	}
	res = call(t, cs, "run_query", map[string]any{
		"conn": strings.TrimSpace(connID), "sql": "SELECT COUNT(*) AS n FROM orders",
	})
	if !strings.Contains(textOf(t, res), "2") {
		t.Fatalf("run_query on saved conn = %q", textOf(t, res))
	}
	if strings.Contains(textOf(t, res), "/a.db") || strings.Contains(res.Content[0].(*mcp.TextContent).Text, "conn-a") {
		t.Fatalf("run_query leaks credential: %q", textOf(t, res))
	}
}

// 无钥匙串后端（#32）：存档两个工具报可读错误，connect 不受影响。
func TestMCPSavedNoKeychain(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)

	for _, name := range []string{"list_connections", "connect_saved"} {
		args := map[string]any{}
		if name == "connect_saved" {
			args["name"] = "x"
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name: name, Arguments: args,
		})
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			msg = textOf(t, res)
		}
		if !strings.Contains(msg, "钥匙串不可用") {
			t.Fatalf("%s without keychain = %q", name, msg)
		}
	}
	connID := connect(t, cs, dsn) // connect 仍可用
	res := call(t, cs, "run_query", map[string]any{
		"conn": connID, "sql": "SELECT COUNT(*) AS n FROM orders",
	})
	if !strings.Contains(textOf(t, res), "2") {
		t.Fatalf("run_query after plain connect = %q", textOf(t, res))
	}
}

// MCP 外键工具（#34）：与界面同一份缓存数据。
func TestMCPListForeignKeys(t *testing.T) {
	home := t.TempDir()
	dsn := setupDB(t, home)
	cs := newClient(t, home, false)
	connID := connect(t, cs, dsn)

	res := call(t, cs, "list_foreign_keys", map[string]any{
		"conn": connID, "database": "main", "table": "items",
	})
	out := textOf(t, res)
	if !strings.Contains(out, "orders") || !strings.Contains(out, "CASCADE") {
		t.Fatalf("list_foreign_keys = %q", out)
	}
}
