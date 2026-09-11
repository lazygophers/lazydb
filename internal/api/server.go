// Package api 是 sidecar 的本机 HTTP+JSON 接口（主测试缝）。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/source"
)

// New 组装路由。token 非空时启用 Bearer 鉴权。
func New(m *conn.Manager, token string) http.Handler {
	s := &server{m: m, token: token}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/connections", s.createConn)
	mux.HandleFunc("GET /api/connections", s.listConns)
	mux.HandleFunc("DELETE /api/connections/{id}", s.removeConn)
	mux.HandleFunc("POST /api/connections/{id}/ping", s.ping)
	mux.HandleFunc("GET /api/connections/{id}/children", s.children)
	mux.HandleFunc("POST /api/connections/{id}/exec", s.exec)
	mux.HandleFunc("GET /api/connections/{id}/columns", s.columns)
	mux.HandleFunc("GET /api/connections/{id}/indexes", s.indexes)
	mux.HandleFunc("GET /api/connections/{id}/ddl", s.ddl)
	mux.HandleFunc("GET /api/connections/{id}/capabilities", s.capabilities)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return s.auth(mux)
}

type server struct {
	m     *conn.Manager
	token string
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
	nodes, err := c.Src.Children(r.Context(), pathParam(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "children_failed", err.Error())
		return
	}
	if nodes == nil {
		nodes = []source.Node{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
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
	res, err := c.Src.Exec(r.Context(), req.SQL, req.ExecOptions)
	if err != nil {
		// SQL 语法/约束错误：客户端的输入问题，回 400 而不是 500
		writeErr(w, http.StatusBadRequest, "sql_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- 能力接口（类型断言探测，ADR-0003）----

func (s *server) columns(w http.ResponseWriter, r *http.Request) {
	c, ok := s.getConn(w, r)
	if !ok {
		return // getConn 已写响应
	}
	cl, ok := c.Src.(source.ColumnLister)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no ColumnLister")
		return
	}
	cols, err := cl.Columns(r.Context(), pathParam(r))
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
	il, ok := c.Src.(source.IndexLister)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no IndexLister")
		return
	}
	idxs, err := il.Indexes(r.Context(), pathParam(r))
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
	ds, ok := c.Src.(source.DDLShower)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "unsupported", "source has no DDLShower")
		return
	}
	ddl, err := ds.DDL(r.Context(), pathParam(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ddl": ddl})
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

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}
