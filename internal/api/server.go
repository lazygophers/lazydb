// Package api 是 sidecar 的本机 HTTP+JSON 接口（主测试缝）。
package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lazygophers/lazydb/internal/audit"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/history"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/xuri/excelize/v2"
)

// New 组装路由。cs 为 nil 时关缓存；token 非空时启用 Bearer 鉴权；
// hist/aud 为 nil 时关历史/审计（测试可注入）。
func New(m *conn.Manager, cs *cache.Store, token string, hist *history.Store, aud *audit.Logger) http.Handler {
	s := &server{m: m, store: cs, token: token, ttl: cache.DefaultTTL, hist: hist, aud: aud}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/connections", s.createConn)
	mux.HandleFunc("GET /api/connections", s.listConns)
	mux.HandleFunc("DELETE /api/connections/{id}", s.removeConn)
	mux.HandleFunc("POST /api/connections/{id}/ping", s.ping)
	mux.HandleFunc("GET /api/connections/{id}/children", s.children)
	mux.HandleFunc("POST /api/connections/{id}/exec", s.exec)
	mux.HandleFunc("POST /api/connections/{id}/export", s.export)
	mux.HandleFunc("GET /api/connections/{id}/columns", s.columns)
	mux.HandleFunc("GET /api/connections/{id}/indexes", s.indexes)
	mux.HandleFunc("GET /api/connections/{id}/ddl", s.ddl)
	mux.HandleFunc("GET /api/connections/{id}/search", s.search)
	mux.HandleFunc("GET /api/connections/{id}/capabilities", s.capabilities)
	mux.HandleFunc("POST /api/connections/{id}/refresh", s.refresh)
	mux.HandleFunc("POST /api/test-connection", s.testConnection)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	if hist != nil {
		mux.HandleFunc("GET /api/history", s.listHistory)
	}

	return s.auth(mux)
}

type server struct {
	m     *conn.Manager
	store *cache.Store
	token string
	ttl   time.Duration
	hist  *history.Store
	aud   *audit.Logger

	idxMu sync.Mutex
	idx   map[string]*searchIndex // connID → 内存索引（refresh 时作废）
}

// ---- 鉴权 ----

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || got != s.token {
				writeErr(w, http.StatusUnauthorized, "unauthorized", "bad or missing bearer token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 结构化错误 ----

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- handlers ----

func (s *server) createConn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string        `json:"name"`
		Cfg  source.Config `json:"config"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
		return
	}
	c, err := s.m.Add(r.Context(), req.Name, req.Cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "open_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *server) listConns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.m.List())
}

func (s *server) removeConn(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Remove(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	s.idxMu.Lock()
	delete(s.idx, r.PathValue("id"))
	s.idxMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	if err := c.Src.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, "ping_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) children(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	nodes, cached, err := cached(s, r.Context(), c, pathParam(r), cache.KindChildren,
		func(ctx context.Context) ([]source.Node, error) {
			return c.Src.Children(ctx, pathParam(r))
		})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "children_failed", err.Error())
		return
	}
	if nodes == nil {
		nodes = []source.Node{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes, "cached": cached})
}

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	var req struct {
		SQL                string `json:"sql"`
		source.ExecOptions        // 嵌入 max_rows
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "sql is required")
		return
	}
	res, err := func() (source.Result, error) {
		start := time.Now()
		res, err := c.Src.Exec(r.Context(), req.SQL, req.ExecOptions)
		s.recordExec("ui", c.ID, req.SQL, time.Since(start), err)
		return res, err
	}()
	if err != nil {
		// SQL 语法/约束错误：客户端的输入问题，回 400 而不是 500
		writeErr(w, http.StatusBadRequest, "sql_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- 导出（#24：RowStreamer 逐行写回，不整包进内存） ----

// export POST {sql, format:csv|xlsx}。流式回包；也是一次执行，留历史与审计。
func (s *server) export(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return
	}
	rs, ok := c.Src.(source.RowStreamer)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "unsupported", "该数据源不支持导出")
		return
	}
	var req struct {
		SQL    string `json:"sql"`
		Format string `json:"format"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "sql is required")
		return
	}
	if req.Format != "csv" && req.Format != "xlsx" {
		writeErr(w, http.StatusBadRequest, "bad_request", "format 仅支持 csv | xlsx")
		return
	}

	w.Header().Set("Content-Type", map[string]string{
		"csv":  "text/csv; charset=utf-8",
		"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	}[req.Format])
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=export.%s", req.Format))

	bw := &countWriter{ResponseWriter: w}
	start := time.Now()
	var err error
	switch req.Format {
	case "csv":
		cw := csv.NewWriter(bw)
		// BOM 在 header 回调里写：SQL 本身失败时一个字节都没出，还能回 400
		err = rs.Stream(r.Context(), req.SQL,
			func(cols []string) error {
				if _, e := bw.Write([]byte{0xEF, 0xBB, 0xBF}); e != nil { // Excel 双击打开才认 UTF-8 中文
					return e
				}
				return cw.Write(cols)
			},
			func(row []any) error { return cw.Write(cellsOf(row)) })
		cw.Flush()
		if err == nil {
			err = cw.Error()
		}
	default:
		var f *excelize.File
		var sw *excelize.StreamWriter
		f = excelize.NewFile()
		sw, err = f.NewStreamWriter("Sheet1")
		if err == nil {
			n := 0
			err = rs.Stream(r.Context(), req.SQL,
				func(cols []string) error {
					n++
					return sw.SetRow("A1", headerIface(cols))
				},
				func(row []any) error {
					n++
					cells := make([]interface{}, len(row))
					for i, v := range row {
						cells[i] = v
					}
					return sw.SetRow(fmt.Sprintf("A%d", n), cells)
				})
			if err == nil {
				if err = sw.Flush(); err == nil {
					err = f.Write(bw)
				}
			}
		}
	}
	s.recordExec("ui", c.ID, "export "+req.Format+": "+req.SQL, time.Since(start), err)
	if err != nil && bw.n == 0 {
		// 一个字节没出才能回错误状态码；已开流只能靠截断的文件让客户端感知
		writeErr(w, http.StatusBadRequest, "export_failed", err.Error())
	}
}

// countWriter 数写到客户端的字节（export 判断还能否回错误状态码）。
type countWriter struct {
	http.ResponseWriter
	n int
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

// cellsOf 行转字符串（NULL → 空）。
func cellsOf(row []any) []string {
	out := make([]string, len(row))
	for i, v := range row {
		switch x := v.(type) {
		case nil:
		case string:
			out[i] = x
		case []byte:
			out[i] = string(x)
		default:
			out[i] = fmt.Sprint(x)
		}
	}
	return out
}

func headerIface(cols []string) []interface{} {
	out := make([]interface{}, len(cols))
	for i, c := range cols {
		out[i] = c
	}
	return out
}

// recordExec 历史与审计共用一条记录（#23）。写失败不拦回包。
func (s *server) recordExec(caller, connID, sql string, d time.Duration, err error) {
	if s.hist == nil && s.aud == nil {
		return
	}
	e := history.Entry{
		TS: time.Now().UnixMilli(), Conn: connID, SQL: sql,
		MS: d.Milliseconds(), OK: err == nil,
	}
	if err != nil {
		e.Err = err.Error()
	}
	_ = s.hist.Save(context.Background(), e)
	_ = s.aud.Log(caller, e)
}

// ---- 搜索（#23：内存索引，断网走缓存 stale-fallback） ----

type searchIndex struct {
	tables  []string // db.table 全名
	columns map[string][]source.Column
}

// searchIndexFor 首次搜索时建内存索引（经缓存，拉不动的库靠 stale-fallback），
// 之后纯内存匹配。refresh 作废重建。
func (s *server) searchIndexFor(ctx context.Context, c *conn.Conn) (*searchIndex, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.idx == nil {
		s.idx = map[string]*searchIndex{}
	}
	if ix, ok := s.idx[c.ID]; ok {
		return ix, nil
	}
	ix := &searchIndex{columns: map[string][]source.Column{}}
	cl, hasCols := c.Src.(source.ColumnLister)

	dbs, _, err := cached(s, ctx, c, nil, cache.KindChildren,
		func(ctx context.Context) ([]source.Node, error) { return c.Src.Children(ctx, nil) })
	if err != nil {
		return nil, err
	}
	for _, db := range dbs {
		tables, _, err := cached(s, ctx, c, source.Path{db.Name}, cache.KindChildren,
			func(ctx context.Context) ([]source.Node, error) {
				return c.Src.Children(ctx, source.Path{db.Name})
			})
		if err != nil {
			continue // 单库失败不拖垮整搜
		}
		for _, t := range tables {
			full := db.Name + "." + t.Name
			ix.tables = append(ix.tables, full)
			if hasCols {
				if cols, _, err := cached(s, ctx, c, source.Path{db.Name, t.Name}, cache.KindColumns,
					func(ctx context.Context) ([]source.Column, error) {
						return cl.Columns(ctx, source.Path{db.Name, t.Name})
					}); err == nil {
					ix.columns[full] = cols
				}
			}
		}
	}
	s.idx[c.ID] = ix
	return ix, nil
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	q := strings.ToLower(r.URL.Query().Get("q"))
	ix, err := s.searchIndexFor(r.Context(), c)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "search_failed", err.Error())
		return
	}
	type match struct {
		Kind     string `json:"kind"` // table | column
		Table    string `json:"table"`
		Column   string `json:"column,omitempty"`
		Type     string `json:"type,omitempty"`
	}
	matches := []match{}
	for _, t := range ix.tables {
		if strings.Contains(strings.ToLower(t), q) {
			matches = append(matches, match{Kind: "table", Table: t})
		}
		for _, col := range ix.columns[t] {
			if q != "" && strings.Contains(strings.ToLower(col.Name), q) {
				matches = append(matches, match{Kind: "column", Table: t, Column: col.Name, Type: col.Type})
			}
		}
	}
	// ponytail: 无上限返回；结果集大到拖慢回包时再加分页。
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// ---- 查询历史 ----

func (s *server) listHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	items, err := s.hist.List(r.Context(), q, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "history_error", err.Error())
		return
	}
	if items == nil {
		items = []history.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ---- 能力接口（类型断言探测，ADR-0003）----

func (s *server) columns(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	cl, ok2 := c.Src.(source.ColumnLister)
	if !ok2 {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no ColumnLister")
		return
	}
	p := pathParam(r)
	cols, _, err := cached(s, r.Context(), c, p, cache.KindColumns,
		func(ctx context.Context) ([]source.Column, error) {
			return cl.Columns(ctx, p)
		})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "sql_error", err.Error())
		return
	}
	if cols == nil {
		cols = []source.Column{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols})
}

func (s *server) indexes(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	il, ok2 := c.Src.(source.IndexLister)
	if !ok2 {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no IndexLister")
		return
	}
	p := pathParam(r)
	idxs, _, err := cached(s, r.Context(), c, p, cache.KindIndexes,
		func(ctx context.Context) ([]source.Index, error) {
			return il.Indexes(ctx, p)
		})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "sql_error", err.Error())
		return
	}
	if idxs == nil {
		idxs = []source.Index{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": idxs})
}

func (s *server) ddl(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	ds, ok2 := c.Src.(source.DDLShower)
	if !ok2 {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no DDLShower")
		return
	}
	p := pathParam(r)
	ddl, _, err := cached(s, r.Context(), c, p, cache.KindDDL,
		func(ctx context.Context) (string, error) {
			return ds.DDL(ctx, p)
		})
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ddl": ddl})
}

// refresh 手动强制刷新：丢缓存后立刻重拉指定 path（表则含字段/索引/DDL）。
func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	var req struct {
		Path []string `json:"path"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if s.store != nil {
		if err := s.store.Drop(r.Context(), c.Key(), req.Path); err != nil {
			writeErr(w, http.StatusInternalServerError, "cache_error", err.Error())
			return
		}
	}
	s.idxMu.Lock() // 结构变了，搜索索引作废
	delete(s.idx, c.ID)
	s.idxMu.Unlock()
	p := source.Path(req.Path)
	refreshed := map[string]bool{}
	if nodes, err := c.Src.Children(r.Context(), p); err == nil && s.store != nil {
		_ = s.store.Save(r.Context(), c.Key(), req.Path, cache.KindChildren, nodes, time.Now())
		refreshed["children"] = true
	}
	if len(p) >= 2 { // 表级：连带字段/索引/DDL
		if cl, ok := c.Src.(source.ColumnLister); ok && s.store != nil {
			if v, err := cl.Columns(r.Context(), p); err == nil {
				_ = s.store.Save(r.Context(), c.Key(), req.Path, cache.KindColumns, v, time.Now())
				refreshed["columns"] = true
			}
		}
		if il, ok := c.Src.(source.IndexLister); ok && s.store != nil {
			if v, err := il.Indexes(r.Context(), p); err == nil {
				_ = s.store.Save(r.Context(), c.Key(), req.Path, cache.KindIndexes, v, time.Now())
				refreshed["indexes"] = true
			}
		}
		if ds, ok := c.Src.(source.DDLShower); ok && s.store != nil {
			if v, err := ds.DDL(r.Context(), p); err == nil {
				_ = s.store.Save(r.Context(), c.Key(), req.Path, cache.KindDDL, v, time.Now())
				refreshed["ddl"] = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": refreshed})
}

func (s *server) capabilities(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	writeJSON(w, http.StatusOK, map[string]bool{
		"columnLister": func() bool { _, ok := c.Src.(source.ColumnLister); return ok }(),
		"indexLister":  func() bool { _, ok := c.Src.(source.IndexLister); return ok }(),
		"ddlShower":    func() bool { _, ok := c.Src.(source.DDLShower); return ok }(),
	})
}

// ---- helpers ----

// testConnection 不登记连接、只验证凭据能否打开并 Ping（「测试连接」按钮）。
func (s *server) testConnection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cfg source.Config `json:"config"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	src, err := s.m.Open(req.Cfg)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := src.Open(r.Context(), req.Cfg); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer src.Close()
	if err := src.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) getConn(w http.ResponseWriter, r *http.Request) (*conn.Conn, bool) {
	c, err := s.m.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return nil, false
	}
	return c, true
}

// pathParam 把重复的 ?path=a&path=b 查询参数拼成 source.Path。
func pathParam(r *http.Request) source.Path {
	return source.Path(r.URL.Query()["path"])
}

// cached 是缓存优先读取（ADR-0004）：新鲜缓存直接回；过期或没有就拉源并写回；
// 源失败但有陈旧缓存 → 回陈旧（离线浏览）。cached=true 表示本次走了缓存。
func cached[T any](s *server, ctx context.Context, c *conn.Conn, path source.Path, kind string, pull func(context.Context) (T, error)) (v T, fromCache bool, err error) {
	if s.store != nil {
		if e, ok, gerr := s.store.Get(ctx, c.Key(), path, kind); gerr == nil && ok && time.Since(e.FetchedAt) < s.ttl {
			if jerr := json.Unmarshal(e.Payload, &v); jerr == nil {
				return v, true, nil
			}
		}
	}
	v, err = pull(ctx)
	if err != nil {
		if s.store != nil {
			if e, ok, gerr := s.store.Get(ctx, c.Key(), path, kind); gerr == nil && ok {
				var stale T
				if jerr := json.Unmarshal(e.Payload, &stale); jerr == nil {
					return stale, true, nil
				}
			}
		}
		return v, false, err
	}
	if s.store != nil {
		_ = s.store.Save(ctx, c.Key(), path, kind, v, time.Now())
	}
	return v, false, nil
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
