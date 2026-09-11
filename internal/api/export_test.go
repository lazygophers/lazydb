// #24 数据写入 + 导出（HTTP 缝）：写语句受影响行数、CSV/XLSX 中文与空值、大结果流式。
package api_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
	"github.com/xuri/excelize/v2"
)

func newWriteServer(t *testing.T, stmts ...string) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	var src sqlsrc.SQLite
	if err := src.Open(t.Context(), source.Config{Driver: "sqlite", DSN: dir + "/w.db"}); err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, s := range stmts {
		if _, err := src.Exec(t.Context(), s, source.ExecOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := conn.NewManager(func(cfg source.Config) (source.Source, error) { return &sqlsrc.SQLite{}, nil })
	ts := httptest.NewServer(api.New(m, store, token, nil, nil))
	t.Cleanup(ts.Close)
	body, _ := json.Marshal(map[string]any{"name": "w", "config": map[string]string{"driver": "sqlite", "dsn": dir + "/w.db"}})
	res := do(t, ts, "POST", "/api/connections", body, http.StatusCreated)
	var c struct{ ID string }
	json.Unmarshal(res, &c)
	return ts, c.ID
}

func exec(t *testing.T, ts *httptest.Server, id, sql string) source.Result {
	t.Helper()
	res := do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": sql}), http.StatusOK)
	var r source.Result
	json.Unmarshal(res, &r)
	return r
}

func export(t *testing.T, ts *httptest.Server, id, sql, format string) []byte {
	t.Helper()
	res := do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/export", id),
		mustJSON(t, map[string]string{"sql": sql, "format": format}), http.StatusOK)
	return res
}

// 写语句返回受影响行数；失败时错误可读。
func TestWriteExecReturnsAffectedRows(t *testing.T) {
	ts, id := newWriteServer(t, `CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`)

	if r := exec(t, ts, id, "INSERT INTO t (v) VALUES ('a'), ('b'), ('c')"); r.RowsAffected != 3 {
		t.Fatalf("insert affected = %+v", r)
	}
	if r := exec(t, ts, id, "UPDATE t SET v = 'x' WHERE id <= 2"); r.RowsAffected != 2 {
		t.Fatalf("update affected = %+v", r)
	}
	if r := exec(t, ts, id, "DELETE FROM t WHERE id = 3"); r.RowsAffected != 1 {
		t.Fatalf("delete affected = %+v", r)
	}
	var r source.Result
	json.Unmarshal(do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": "SELECT COUNT(*) AS n FROM t"}), http.StatusOK), &r)
	if fmt.Sprint(r.Rows[0][0]) != "2" {
		t.Fatalf("count after writes = %+v", r.Rows)
	}

	// 失败可读：唯一约束等错误回 400 + 原因
	res := do(t, ts, "POST", fmt.Sprintf("/api/connections/%s/exec", id),
		mustJSON(t, map[string]string{"sql": "INSERT INTO nope (x) VALUES (1)"}), http.StatusBadRequest)
	if !strings.Contains(string(res), "no such table") {
		t.Fatalf("error = %s", res)
	}
}

// CSV：BOM、中文、空值（NULL → 空）、逗号转义。
func TestExportCSVChineseAndNull(t *testing.T) {
	ts, id := newWriteServer(t,
		`CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT, note TEXT)`,
		`INSERT INTO p (name, note) VALUES ('张三', '含,逗号'), ('李四', NULL)`)

	b := export(t, ts, id, "SELECT id, name, note FROM p ORDER BY id", "csv")
	if !bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("no UTF-8 BOM")
	}
	recs, err := csv.NewReader(bytes.NewReader(b[3:])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"id", "name", "note"}, {"1", "张三", "含,逗号"}, {"2", "李四", ""}}
	if fmt.Sprint(recs) != fmt.Sprint(want) {
		t.Fatalf("csv = %q", recs)
	}
}

// XLSX：excelize 读回，中文与空值正确。
func TestExportXLSXReadBack(t *testing.T) {
	ts, id := newWriteServer(t,
		`CREATE TABLE p (id INTEGER PRIMARY KEY, name TEXT, note TEXT)`,
		`INSERT INTO p (name, note) VALUES ('张三', 'x'), ('李四', NULL)`)

	f, err := excelize.OpenReader(bytes.NewReader(export(t, ts, id, "SELECT id, name, note FROM p ORDER BY id", "xlsx")))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if cell, _ := f.GetCellValue("Sheet1", "B2"); cell != "张三" {
		t.Fatalf("B2 = %q", cell)
	}
	if cell, _ := f.GetCellValue("Sheet1", "C3"); cell != "" {
		t.Fatalf("NULL cell = %q", cell)
	}
}

// 大结果流式：10 万行 CSV 导出完整（服务端不整包进内存——RowStreamer 逐行回调）。
func TestExportLargeResultStreamed(t *testing.T) {
	ts, id := newWriteServer(t, `CREATE TABLE big (n INTEGER)`)
	exec(t, ts, id, `WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 100000)
		INSERT INTO big (n) SELECT n FROM seq`)
	b := export(t, ts, id, "SELECT n FROM big", "csv")
	n := 0
	for _, line := range strings.Split(string(b[3:]), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	if n != 100001 { // 表头 + 10 万行
		t.Fatalf("lines = %d", n)
	}
}
