// Package api 是 sidecar 的本机 HTTP+JSON 接口（主测试缝）。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/source"
)

// New 组装路由。cs 为 nil 时关缓存；token 非空时启用 Bearer 鉴权。
func New(m *conn.Manager, cs *cache.Store, token string) http.Handler {
	s := &server{m: m, store: cs, token: token, ttl: cache.DefaultTTL}
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
	mux.HandleFunc("POST /api/connections/{id}/refresh", s.refresh)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	return s.auth(mux)
}

type server struct {
	m     *conn.Manager
	store *cache.Store
	token string
	ttl   time.Duration
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
