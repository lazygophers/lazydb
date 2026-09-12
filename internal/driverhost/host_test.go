// driverhost 全链路（辅缝：stdio JSON）。子进程 = 本测试二进制 re-exec
// （LAZYDB_DRIVER_CHILD=1 时 TestMain 跑 driveragent，不走测试）。
// 索引用 httptest 本地起，产物 = 测试二进制自身，下载/校验/拉起都是真路径。
package driverhost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lazygophers/lazydb/internal/driveragent"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

func TestMain(m *testing.M) {
	if os.Getenv("LAZYDB_DRIVER_CHILD") == "1" {
		driveragent.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// serveIndex 起一个本地索引 + 产物下载服务，产物就是本测试二进制。
// mutate 在服务已起后改索引（可引用 base = 服务根地址）。
func serveIndex(t *testing.T, mutate func(idx *Index, base string)) (url string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	idx := &Index{Drivers: map[string]Driver{
		"sqlite": {
			Version:  "1.0.0",
			Protocol: [2]int{1, 2},
			Platforms: map[string]Artifact{
				goosArch(): {SHA256: sha256hex(bin)},
			},
		},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/index", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(idx)
	})
	mux.HandleFunc("/artifact", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bin)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	plats := idx.Drivers["sqlite"].Platforms
	plats[goosArch()] = Artifact{URL: ts.URL + "/artifact", SHA256: sha256hex(bin)}
	if mutate != nil {
		mutate(idx, ts.URL)
	}
	return ts.URL + "/index"
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func goosArch() string { return runtime.GOOS + "/" + runtime.GOARCH }

func newHost(t *testing.T, indexURL string, idle time.Duration) *Host {
	t.Helper()
	h := New(t.TempDir())
	h.IndexURL = indexURL
	h.IdleTimeout = idle
	h.Spawn = func(path string, args ...string) *exec.Cmd {
		cmd := exec.Command(path, args...)
		cmd.Env = append(os.Environ(), "LAZYDB_DRIVER_CHILD=1")
		return cmd
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func setupDB(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	dsn := filepath.Join(home, "plug.db")
	var src sqlsrc.SQLite
	if err := src.Open(context.Background(), source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, stmt := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX idx_c ON orders(customer)`,
		`INSERT INTO orders (customer) VALUES ('alice'), ('bob')`,
	} {
		if _, err := src.Exec(context.Background(), stmt, source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return dsn
}

// 从索引下载 → 哈希校验 → 拉起 → open 握手 → 全部只读方法 → 关闭回收。
func TestPluginFullChain(t *testing.T) {
	url := serveIndex(t, nil)
	h := newHost(t, url, 0)
	dsn := setupDB(t)

	// 下载发生前本地没有驱动
	if _, err := os.Stat(filepath.Join(h.driverDir(), "sqlite")); !os.IsNotExist(err) {
		t.Fatalf("driver should not exist yet: %v", err)
	}

	src, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}

	nodes, err := src.Children(context.Background(), nil)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "main" {
		t.Fatalf("children = %+v err=%v", nodes, err)
	}
	tables, err := src.Children(context.Background(), source.Path{"main"})
	if err != nil || len(tables) != 1 || tables[0].Name != "orders" {
		t.Fatalf("tables = %+v err=%v", tables, err)
	}
	res, err := src.Exec(context.Background(), "SELECT customer FROM orders ORDER BY id", source.ExecOptions{})
	if err != nil || len(res.Rows) != 2 || res.Rows[0][0] != "alice" {
		t.Fatalf("exec = %+v err=%v", res, err)
	}

	// 能力装配：sqlite 三个都支持
	cl, ok := src.(source.ColumnLister)
	if !ok {
		t.Fatal("ColumnLister not assembled")
	}
	cols, err := cl.Columns(context.Background(), source.Path{"main", "orders"})
	if err != nil || len(cols) != 2 || cols[1].Nullable {
		t.Fatalf("columns = %+v err=%v", cols, err)
	}
	il, ok := src.(source.IndexLister)
	if !ok {
		t.Fatal("IndexLister not assembled")
	}
	idxs, err := il.Indexes(context.Background(), source.Path{"main", "orders"})
	if err != nil || len(idxs) == 0 {
		t.Fatalf("indexes = %+v err=%v", idxs, err)
	}
	ds, ok := src.(source.DDLShower)
	if !ok {
		t.Fatal("DDLShower not assembled")
	}
	ddl, err := ds.DDL(context.Background(), source.Path{"main", "orders"})
	if err != nil || !strings.Contains(ddl, "CREATE TABLE") {
		t.Fatalf("ddl = %q err=%v", ddl, err)
	}

	// Close 回收进程
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("clients after close = %d", n)
	}
}

// 空闲超时自动退出。
func TestIdleReap(t *testing.T) {
	url := serveIndex(t, nil)
	h := newHost(t, url, 400*time.Millisecond)
	dsn := setupDB(t)

	src, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	c := src.(*client)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.exited() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !c.exited() {
		t.Fatal("driver process not reaped after idle timeout")
	}
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("clients after reap = %d", n)
	}
}

// 后端退出全量回收，无僵尸。
func TestHostCloseKillsAll(t *testing.T) {
	url := serveIndex(t, nil)
	h := newHost(t, url, 0)
	dsn := setupDB(t)

	var cs []*client
	for i := 0; i < 3; i++ {
		src, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, src.(*client))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	for i, c := range cs {
		if !c.exited() {
			t.Fatalf("client %d still alive after host close", i)
		}
	}
}

// 索引哈希不符拒绝加载。
func TestHashMismatchRejected(t *testing.T) {
	url := serveIndex(t, func(idx *Index, base string) {
		idx.Drivers["sqlite"].Platforms[goosArch()] = Artifact{
			URL:    base + "/artifact",
			SHA256: strings.Repeat("0", 64),
		}
	})
	h := newHost(t, url, 0)
	_, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: "x.db"})
	if err == nil || !strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("err = %v", err)
	}
}

// 索引声明协议不兼容拒绝加载。
func TestProtocolIncompatibleRejected(t *testing.T) {
	url := serveIndex(t, func(idx *Index, base string) {
		drv := idx.Drivers["sqlite"]
		drv.Protocol = [2]int{3, 4}
		idx.Drivers["sqlite"] = drv
	})
	h := newHost(t, url, 0)
	_, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: "x.db"})
	if err == nil || !strings.Contains(err.Error(), "不兼容") {
		t.Fatalf("err = %v", err)
	}
}

// 无索引、无网络：本地已装驱动（= 内置 tracer bullet 路径）不受影响。
func TestOfflineLocalDriverWorks(t *testing.T) {
	h := newHost(t, "", 0)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.driverDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join(h.driverDir(), "sqlite"), self); err != nil {
		t.Fatal(err)
	}
	dsn := setupDB(t)
	src, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(context.Background(), "SELECT 1", source.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
}

func url0(s string) string { return s }

func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

// 只读执行经驱动代理全链路（#30）：WITH … DELETE 被事务回滚，数据不变。
func TestPluginExecReadOnly(t *testing.T) {
	url := serveIndex(t, nil)
	h := newHost(t, url, 0)
	dsn := setupDB(t)

	src, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	ro, ok := src.(source.ReadOnlyExecer)
	if !ok {
		t.Fatal("ReadOnlyExecer not assembled")
	}
	if _, err := ro.ExecReadOnly(context.Background(),
		"WITH v AS (SELECT 1) DELETE FROM orders", source.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := src.Exec(context.Background(), "SELECT COUNT(*) FROM orders", source.ExecOptions{})
	if err != nil || len(res.Rows) != 1 || fmt.Sprint(res.Rows[0][0]) != "2" {
		t.Fatalf("count after rollback = %+v err=%v", res, err)
	}
}

// 索引 URL 优先级（#33）：设置 > 环境变量 > 默认。
func TestResolveIndexURL(t *testing.T) {
	for _, c := range []struct{ set, env, want string }{
		{"http://s", "http://e", "http://s"}, // 设置压过环境变量
		{"", "http://e", "http://e"},
		{"", "", DefaultIndexURL},
	} {
		if got := ResolveIndexURL(c.set, c.env); got != c.want {
			t.Fatalf("ResolveIndexURL(%q,%q) = %q, want %q", c.set, c.env, got, c.want)
		}
	}
}

// 索引不可达（离线）但本地已装：不联网直接用（#33）。
func TestOfflineWithInstalledDriverWorks(t *testing.T) {
	// 先经真索引装好本地驱动
	url := serveIndex(t, nil)
	h := newHost(t, url, 0)
	dsn := setupDB(t)
	if _, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	h.Close()

	// 换成不可达索引：同一个 Home（本地驱动还在），仍能用
	h2 := &Host{Home: h.Home, clients: map[*client]struct{}{}, IndexURL: "http://127.0.0.1:1/none"}
	h2.Spawn = h.Spawn
	t.Cleanup(func() { _ = h2.Close() })
	src, err := h2.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec(context.Background(), "SELECT 1", source.ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = src.Close()
}

// 外键能力经驱动代理透传（#34）。
func TestPluginForeignKeys(t *testing.T) {
	url := serveIndex(t, nil)
	h := newHost(t, url, 0)

	home := t.TempDir()
	dsn := filepath.Join(home, "fk.db")
	var src sqlsrc.SQLite
	if err := src.Open(context.Background(), source.Config{Driver: "sqlite", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, customer TEXT NOT NULL)`,
		`CREATE TABLE items (id INTEGER PRIMARY KEY, orders_id INTEGER REFERENCES orders(id) ON DELETE CASCADE)`,
	} {
		if _, err := src.Exec(context.Background(), stmt, source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	src.Close()

	c, err := h.Open(context.Background(), "sqlite", source.Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	fl, ok := c.(source.ForeignKeyLister)
	if !ok {
		t.Fatal("ForeignKeyLister not assembled")
	}
	fks, err := fl.ForeignKeys(context.Background(), source.Path{"main", "items"})
	if err != nil || len(fks) != 1 || fks[0].RefTable != "orders" || fks[0].OnDelete != "CASCADE" {
		t.Fatalf("foreign keys = %+v err=%v", fks, err)
	}
	_ = c.Close()
}
