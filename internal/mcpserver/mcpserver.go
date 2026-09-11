// Package mcpserver 是 `lazydb mcp-server` 子命令的实现（ADR-0005）：
// AI 客户端经 stdio 调用，默认拒绝一切写语句，--allow-write 显式开启。
// 与桌面后端共用 ~/.lazydb/cache.db（ADR-0004 跨进程契约，WAL 并发安全）。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lazygophers/lazydb/drivers/builtin"
	"github.com/lazygophers/lazydb/internal/audit"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/history"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run 启动 stdio MCP server，阻塞直到客户端断开。
func Run(ctx context.Context, home string, allowWrite bool) error {
	store, err := cache.Open(home)
	if err != nil {
		return fmt.Errorf("open cache.db: %w", err)
	}
	defer store.Close()
	hist, err := history.Open(home)
	if err != nil {
		return fmt.Errorf("open history.db: %w", err)
	}
	defer hist.Close()
	aud, err := audit.Open(home)
	if err != nil {
		return fmt.Errorf("open audit.log: %w", err)
	}
	defer aud.Close()

	srv := mcp.NewServer(&mcp.Implementation{Name: "lazydb", Version: "v1"}, nil)
	h := &hub{m: conn.NewManager(builtin.Open), store: store, allowWrite: allowWrite, hist: hist, aud: aud}

	h.toolConnect(srv)
	h.toolListConnections(srv)
	h.toolListDatabases(srv)
	h.toolListTables(srv)
	h.toolListColumns(srv)
	h.toolGetDDL(srv)
	h.toolSearchSchema(srv)
	h.toolRunQuery(srv)

	return srv.Run(ctx, &mcp.StdioTransport{})
}

// hub 汇聚 handler 共用的状态。TTL 与 HTTP 后端一致。
type hub struct {
	m          *conn.Manager
	store      *cache.Store
	allowWrite bool
	hist       *history.Store
	aud        *audit.Logger
}

func (h *hub) toolConnect(s *mcp.Server) {
	type args struct {
		Name   string `json:"name" jsonschema:"显示名"`
		Driver string `json:"driver" jsonschema:"sqlite | mysql"`
		DSN    string `json:"dsn" jsonschema:"数据源连接串（格式由驱动解释）"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "connect",
		Description: "连接一个数据源，返回后续工具用的 conn_id",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Add(ctx, a.Name, source.Config{Driver: a.Driver, DSN: a.DSN})
		if err != nil {
			return nil, nil, err
		}
		if err := c.Src.Ping(ctx); err != nil {
			_ = h.m.Remove(c.ID)
			return nil, nil, fmt.Errorf("ping: %w", err)
		}
		return text(fmt.Sprintf("conn_id: %s", c.ID)), nil, nil
	})
}

func (h *hub) toolListConnections(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_connections", Description: "列出本会话已建立的连接",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		var b strings.Builder
		for _, c := range h.m.List() {
			fmt.Fprintf(&b, "%s\t%s\t%s\n", c.ID, c.Name, c.Cfg.Driver)
		}
		return text(b.String()), nil, nil
	})
}

func (h *hub) toolListDatabases(s *mcp.Server) {
	type args struct {
		Conn string `json:"conn"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_databases", Description: "列出库（结构树根层）",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		nodes, _, err := cachedNodes(ctx, h, c, nil)
		if err != nil {
			return nil, nil, err
		}
		return text(nodeLines(nodes)), nil, nil
	})
}

func (h *hub) toolListTables(s *mcp.Server) {
	type args struct {
		Conn     string `json:"conn"`
		Database string `json:"database"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_tables", Description: "列出库下的表和视图",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		nodes, _, err := cachedNodes(ctx, h, c, source.Path{a.Database})
		if err != nil {
			return nil, nil, err
		}
		return text(nodeLines(nodes)), nil, nil
	})
}

func (h *hub) toolListColumns(s *mcp.Server) {
	type args struct {
		Conn     string `json:"conn"`
		Database string `json:"database"`
		Table    string `json:"table"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "list_columns", Description: "列出表的字段（名字/类型/可空）",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		cl, ok := c.Src.(source.ColumnLister)
		if !ok {
			return nil, nil, fmt.Errorf("该数据源不支持列字段")
		}
		p := source.Path{a.Database, a.Table}
		cols, _, err := cachedJSON(ctx, h, c, p, cache.KindColumns,
			func(ctx context.Context) ([]source.Column, error) { return cl.Columns(ctx, p) })
		if err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		for _, col := range cols {
			nullable := "NULL"
			if !col.Nullable {
				nullable = "NOT NULL"
			}
			fmt.Fprintf(&b, "%s\t%s\t%s\n", col.Name, col.Type, nullable)
		}
		return text(b.String()), nil, nil
	})
}

func (h *hub) toolGetDDL(s *mcp.Server) {
	type args struct {
		Conn     string `json:"conn"`
		Database string `json:"database"`
		Object   string `json:"object"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_ddl", Description: "取表/视图的建表语句",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		ds, ok := c.Src.(source.DDLShower)
		if !ok {
			return nil, nil, fmt.Errorf("该数据源不支持 DDL")
		}
		p := source.Path{a.Database, a.Object}
		ddl, _, err := cachedJSON(ctx, h, c, p, cache.KindDDL,
			func(ctx context.Context) (string, error) { return ds.DDL(ctx, p) })
		if err != nil {
			return nil, nil, err
		}
		return text(ddl), nil, nil
	})
}

// toolSearchSchema：名字模糊搜索（ADR #5：只搜结构名，不搜表内数据）。
// ponytail: 逐库拉表名后内存过滤；表数量大到拉取慢时换缓存内索引。
func (h *hub) toolSearchSchema(s *mcp.Server) {
	type args struct {
		Conn     string `json:"conn"`
		Database string `json:"database,omitempty" jsonschema:"留空则搜所有库"`
		Query    string `json:"query" jsonschema:"模糊匹配的子串"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "search_schema", Description: "按名字模糊搜索当前连接的结构（库/表）",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		q := strings.ToLower(a.Query)
		var b strings.Builder
		dbs := []string{a.Database}
		if a.Database == "" {
			nodes, _, err := cachedNodes(ctx, h, c, nil)
			if err != nil {
				return nil, nil, err
			}
			dbs = dbs[:0]
			for _, n := range nodes {
				dbs = append(dbs, n.Name)
			}
		}
		for _, db := range dbs {
			if q != "" && strings.Contains(strings.ToLower(db), q) {
				fmt.Fprintf(&b, "%s\t(database)\n", db)
			}
			nodes, _, err := cachedNodes(ctx, h, c, source.Path{db})
			if err != nil {
				continue // 单库失败不拖垮整搜
			}
			for _, n := range nodes {
				if q == "" || strings.Contains(strings.ToLower(n.Name), q) {
					fmt.Fprintf(&b, "%s.%s\t(%s)\n", db, n.Name, n.Kind)
				}
			}
		}
		return text(b.String()), nil, nil
	})
}

// 只读动词白名单。ponytail: 首词判断挡不住 CTE 藏写等绕过，真闸门
// 在 #24 上只读事务；到时换 db.Connector 只读模式。
var readOnlyVerbs = map[string]bool{
	"select": true, "show": true, "desc": true, "describe": true,
	"explain": true, "with": true, "pragma": true, "use": true,
}

func (h *hub) toolRunQuery(s *mcp.Server) {
	type args struct {
		Conn    string `json:"conn"`
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "run_query", Description: "执行 SQL。默认只读：仅 SELECT/SHOW/DESCRIBE/EXPLAIN/WITH/PRAGMA 放行",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a args) (*mcp.CallToolResult, any, error) {
		c, err := h.m.Get(a.Conn)
		if err != nil {
			return nil, nil, err
		}
		if !h.allowWrite && !isReadOnly(a.SQL) {
			return nil, nil, fmt.Errorf(
				"写语句被拒绝（默认只读）。需要写入请让宿主以 --allow-write 启动 lazydb mcp-server")
		}
		if a.MaxRows <= 0 {
			a.MaxRows = 100
		}
		start := time.Now()
		res, err := c.Src.Exec(ctx, a.SQL, source.ExecOptions{MaxRows: a.MaxRows})
		e := history.Entry{
			TS: time.Now().UnixMilli(), Conn: c.ID, SQL: a.SQL,
			MS: time.Since(start).Milliseconds(), OK: err == nil,
		}
		if err != nil {
			e.Err = err.Error()
		}
		_ = h.hist.Save(ctx, e)
		_ = h.aud.Log("mcp", e) // 审计覆盖 MCP 路径（#23）
		if err != nil {
			return nil, nil, err
		}
		return text(renderResult(res)), nil, nil
	})
}

func isReadOnly(sql string) bool {
	s := strings.TrimSpace(sql)
	if i := strings.IndexAny(s, " \t\r\n;"); i >= 0 {
		s = s[:i]
	}
	return readOnlyVerbs[strings.ToLower(s)]
}

// cachedNodes / cachedJSON 与 api.cached 同一套缓存优先逻辑（ADR-0004）。
func cachedNodes(ctx context.Context, h *hub, c *conn.Conn, path source.Path) ([]source.Node, bool, error) {
	return cachedJSON(ctx, h, c, path, cache.KindChildren,
		func(ctx context.Context) ([]source.Node, error) { return c.Src.Children(ctx, path) })
}

func cachedJSON[T any](ctx context.Context, h *hub, c *conn.Conn, path source.Path, kind string, pull func(context.Context) (T, error)) (v T, fromCache bool, err error) {
	if e, ok, gerr := h.store.Get(ctx, c.Key(), path, kind); gerr == nil && ok {
		if jerr := json.Unmarshal(e.Payload, &v); jerr == nil {
			return v, true, nil
		}
	}
	v, err = pull(ctx)
	if err != nil {
		if e, ok, gerr := h.store.Get(ctx, c.Key(), path, kind); gerr == nil && ok {
			var stale T
			if jerr := json.Unmarshal(e.Payload, &stale); jerr == nil {
				return stale, true, nil
			}
		}
		return v, false, err
	}
	_ = h.store.Save(ctx, c.Key(), path, kind, v, time.Now())
	return v, false, nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func nodeLines(nodes []source.Node) string {
	var b strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&b, "%s\t(%s)\n", n.Name, n.Kind)
	}
	return b.String()
}

func renderResult(res source.Result) string {
	var b strings.Builder
	fmt.Fprintln(&b, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = fmt.Sprint(v)
		}
		fmt.Fprintln(&b, strings.Join(cells, "\t"))
	}
	if res.Truncated {
		fmt.Fprintln(&b, "…（已按 max_rows 截断）")
	}
	return b.String()
}
