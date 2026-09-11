// MySQL + SSH 隧道集成测试（HTTP 缝，与 SQLite 同一套抽象）。
// 无环境变量则跳过：CI 设 LAZYDB_MYSQL_DSN 等，本地用 docker 起环境再设。
package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lazygophers/lazydb/drivers/builtin"
	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/source"
)

// mysqlEnv 从环境变量取 MySQL 直连 DSN；没设则跳过。
func mysqlEnv(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LAZYDB_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAZYDB_MYSQL_DSN not set; start MySQL (docker run mysql:8) to run this test")
	}
	return dsn
}

// sshEnv 取 SSH 跳板参数；缺任一则跳过。
func sshEnv(t *testing.T) *source.SSHConfig {
	t.Helper()
	host := os.Getenv("LAZYDB_SSH_HOST")
	key := os.Getenv("LAZYDB_SSH_KEY")
	if host == "" || key == "" {
		t.Skip("LAZYDB_SSH_HOST/LAZYDB_SSH_KEY not set; skipping tunnel test")
	}
	port := 22
	if p := os.Getenv("LAZYDB_SSH_PORT"); p != "" {
		fmt.Sscan(p, &port)
	}
	targetHost := os.Getenv("LAZYDB_SSH_TARGET_HOST")
	if targetHost == "" {
		targetHost = os.Getenv("LAZYDB_MYSQL_HOST")
	}
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	targetPort := 3306
	if p := os.Getenv("LAZYDB_SSH_TARGET_PORT"); p != "" {
		fmt.Sscan(p, &targetPort)
	}
	return &source.SSHConfig{
		Host: host, Port: port,
		User:       envOr("LAZYDB_SSH_USER", "root"),
		KeyPath:    key,
		TargetHost: targetHost, TargetPort: targetPort,
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// newSuiteServer 建「builtin 注册表」后端（与 main 同构），建好连接返回 id。
func newSuiteServer(t *testing.T, cfg source.Config) (ts *httptest.Server, id string) {
	t.Helper()
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := conn.NewManager(builtin.Open)
	ts = httptest.NewServer(api.New(m, store, token))
	t.Cleanup(ts.Close)
	body, _ := json.Marshal(map[string]any{"name": "s", "config": cfg})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)
	return ts, c.ID
}

// 直连 MySQL 8 全链路：连接 → 树 → 字段/索引/DDL → 执行 SQL。
func TestMySQLFullChainDirect(t *testing.T) {
	dsn := mysqlEnv(t)
	ts, id := newSuiteServer(t, source.Config{Driver: "mysql", DSN: dsn})

	// Ping
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/ping", id), nil, http.StatusOK)

	// 建表 + 数据（TRUNCATE 保证重跑幂等）
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `CREATE TABLE IF NOT EXISTS users (
			id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(50) NOT NULL, email VARCHAR(50),
			KEY idx_name (name))`}), http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `TRUNCATE TABLE users`}), http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `INSERT INTO users (name, email) VALUES ('alice','a@x.com')`}), http.StatusOK)

	// 根 → 库列表（用 DSN 里缺省库）
	rest := dsn[strings.LastIndex(dsn, "/")+1:]
	dbName := strings.SplitN(rest, "?", 2)[0]

	var dbs struct {
		Nodes []source.Node `json:"nodes"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/children", id), nil, http.StatusOK), &dbs)
	found := false
	for _, n := range dbs.Nodes {
		if n.Name == dbName && n.Kind == "database" {
			found = true
		}
	}
	if !found {
		t.Fatalf("database %q not in children: %+v", dbName, dbs.Nodes)
	}

	// 库 → 表
	var tables struct {
		Nodes []source.Node `json:"nodes"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/children?path=%s", id, dbName), nil, http.StatusOK), &tables)
	if len(tables.Nodes) != 1 || tables.Nodes[0].Name != "users" || tables.Nodes[0].Kind != "table" {
		t.Fatalf("tables = %+v", tables.Nodes)
	}

	// 字段
	var cols struct {
		Columns []source.Column `json:"columns"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/columns?path=%s&path=users", id, dbName), nil, http.StatusOK), &cols)
	if len(cols.Columns) != 3 || cols.Columns[1].Nullable {
		t.Fatalf("columns = %+v", cols.Columns)
	}

	// 索引（PRIMARY + idx_name）
	var idxs struct {
		Indexes []source.Index `json:"indexes"`
	}
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/indexes?path=%s&path=users", id, dbName), nil, http.StatusOK), &idxs)
	if len(idxs.Indexes) < 2 {
		t.Fatalf("indexes = %+v", idxs.Indexes)
	}

	// DDL
	var ddl struct{ DDL string }
	json.Unmarshal(do(t, ts, "GET", fmt.Sprintf("/api/connections/%s/ddl?path=%s&path=users", id, dbName), nil, http.StatusOK), &ddl)
	if !strings.Contains(ddl.DDL, "CREATE TABLE") {
		t.Fatalf("ddl = %q", ddl.DDL)
	}

	// 执行 SQL
	var res source.Result
	json.Unmarshal(do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT id, name FROM users", "max_rows": 10}), http.StatusOK), &res)
	if len(res.Rows) != 1 || res.Rows[0][1] != "alice" {
		t.Fatalf("exec = %+v", res)
	}
}

// SSH 隧道路径同样打通。
func TestMySQLFullChainTunnel(t *testing.T) {
	dsn := mysqlEnv(t)
	ssh := sshEnv(t)
	ts, id := newSuiteServer(t, source.Config{Driver: "mysql", DSN: dsn, SSH: ssh})

	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/ping", id), nil, http.StatusOK)
	do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": `CREATE TABLE IF NOT EXISTS tunnel_t (x INT PRIMARY KEY)`}), http.StatusOK)
	var res source.Result
	json.Unmarshal(do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]any{"sql": "SELECT COUNT(*) AS n FROM tunnel_t"}), http.StatusOK), &res)
	if len(res.Rows) != 1 {
		t.Fatalf("tunnel exec = %+v", res)
	}
}

// 「测试连接」：对/错凭据分别返回明确结果。
func TestTestConnectionEndpoint(t *testing.T) {
	dsn := mysqlEnv(t)
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := conn.NewManager(builtin.Open)
	ts := httptest.NewServer(api.New(m, store, token))
	t.Cleanup(ts.Close)

	// 对的凭据
	res := do(t, ts, "POST", "/api/test-connection",
		mustJSON(t, map[string]any{"config": map[string]string{"driver": "mysql", "dsn": dsn}}), http.StatusOK)
	var ok struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.Unmarshal(res, &ok)
	if !ok.OK {
		t.Fatalf("good creds: ok=%v error=%s", ok.OK, ok.Error)
	}

	// 错的凭据（密码不对）
	bad := strings.Replace(dsn, ":root@", ":wrongpass@", 1)
	if bad == dsn { // DSN 里没带 root 密码就跳过这半
		t.Skip("dsn without inline root credentials; cannot fabricate bad creds")
	}
	res = do(t, ts, "POST", "/api/test-connection",
		mustJSON(t, map[string]any{"config": map[string]string{"driver": "mysql", "dsn": bad}}), http.StatusOK)
	json.Unmarshal(res, &ok)
	if ok.OK || ok.Error == "" {
		t.Fatalf("bad creds: ok=%v error=%q", ok.OK, ok.Error)
	}
}
