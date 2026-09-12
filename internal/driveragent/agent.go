// Package driveragent 是驱动代理进程的服务端（ADR-0002）：
// 一种数据源一个独立二进制，主程序经 stdio 行分隔 JSON 调用，
// 与 MCP 子命令同构。cmd/lazydb-driver 是它的薄封装。
package driveragent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lazygophers/lazydb/drivers/builtin"
	"github.com/lazygophers/lazydb/internal/source"
)

// ProtocolVersion 与主程序（driverhost）的协议版本协商基准。
// v2（#30）：open 结果加 readonly 能力位，新增 exec_readonly 方法。
const ProtocolVersion = 2

type request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     int    `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Run 阻塞服务 stdio 直到 stdin 关闭。一个代理进程只服务一个数据源连接：
// 首个请求必须是 open，带完整连接配置。
func Run(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024) // 万行结果也放得下

	var src source.Source
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var req request
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			_ = enc.Encode(response{ID: -1, Error: fmt.Sprintf("bad request: %v", err)})
			continue
		}
		if src == nil && req.Method != "open" {
			_ = enc.Encode(response{ID: req.ID, Error: "first request must be open"})
			continue
		}
		res, errMsg := handle(ctx, &src, req)
		if errMsg != "" {
			_ = enc.Encode(response{ID: req.ID, Error: errMsg})
			continue
		}
		_ = enc.Encode(response{ID: req.ID, Result: res})
	}
	return sc.Err()
}

func handle(ctx context.Context, psrc *source.Source, req request) (any, string) {
	if req.Method == "open" {
		var cfg source.Config
		if err := json.Unmarshal(req.Params, &cfg); err != nil {
			return nil, err.Error()
		}
		src, err := builtin.Open(cfg)
		if err != nil {
			return nil, err.Error()
		}
		if err := src.Open(ctx, cfg); err != nil {
			return nil, err.Error()
		}
		*psrc = src
		return map[string]any{
			"protocol":  ProtocolVersion,
			"columns":   supports(src, (*source.ColumnLister)(nil)),
			"indexes":   supports(src, (*source.IndexLister)(nil)),
			"ddl":       supports(src, (*source.DDLShower)(nil)),
			"readonly":  supports(src, (*source.ReadOnlyExecer)(nil)),
			"fkeys":     supports(src, (*source.ForeignKeyLister)(nil)),
		}, ""
	}
	src := *psrc

	switch req.Method {
	case "close":
		if err := src.Close(); err != nil {
			return nil, err.Error()
		}
		*psrc = nil
		return map[string]bool{"ok": true}, ""
	case "ping":
		return map[string]bool{"ok": true}, callErr(src.Ping(ctx))
	case "children":
		var p struct {
			Path source.Path `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		nodes, err := src.Children(ctx, p.Path)
		if err != nil {
			return nil, err.Error()
		}
		if nodes == nil {
			nodes = []source.Node{}
		}
		return map[string]any{"nodes": nodes}, ""
	case "exec":
		var p struct {
			SQL     string `json:"sql"`
			MaxRows int    `json:"max_rows"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		res, err := src.Exec(ctx, p.SQL, source.ExecOptions{MaxRows: p.MaxRows})
		if err != nil {
			return nil, err.Error()
		}
		return res, ""
	case "exec_readonly":
		ro, ok := src.(source.ReadOnlyExecer)
		if !ok {
			return nil, "unsupported: exec_readonly"
		}
		var p struct {
			SQL     string `json:"sql"`
			MaxRows int    `json:"max_rows"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		res, err := ro.ExecReadOnly(ctx, p.SQL, source.ExecOptions{MaxRows: p.MaxRows})
		if err != nil {
			return nil, err.Error()
		}
		return res, ""
	case "columns":
		cl, ok := src.(source.ColumnLister)
		if !ok {
			return nil, "unsupported: columns"
		}
		var p struct {
			Path source.Path `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		cols, err := cl.Columns(ctx, p.Path)
		if err != nil {
			return nil, err.Error()
		}
		return map[string]any{"columns": cols}, ""
	case "indexes":
		il, ok := src.(source.IndexLister)
		if !ok {
			return nil, "unsupported: indexes"
		}
		var p struct {
			Path source.Path `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		idxs, err := il.Indexes(ctx, p.Path)
		if err != nil {
			return nil, err.Error()
		}
		return map[string]any{"indexes": idxs}, ""
	case "foreign_keys":
		fl, ok := src.(source.ForeignKeyLister)
		if !ok {
			return nil, "unsupported: foreign_keys"
		}
		var p struct {
			Path source.Path `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		fks, err := fl.ForeignKeys(ctx, p.Path)
		if err != nil {
			return nil, err.Error()
		}
		return map[string]any{"foreign_keys": fks}, ""
	case "ddl":
		ds, ok := src.(source.DDLShower)
		if !ok {
			return nil, "unsupported: ddl"
		}
		var p struct {
			Path source.Path `json:"path"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, err.Error()
		}
		ddl, err := ds.DDL(ctx, p.Path)
		if err != nil {
			return nil, err.Error()
		}
		return map[string]any{"ddl": ddl}, ""
	default:
		return nil, fmt.Sprintf("unknown method: %s", req.Method)
	}
}

// supports 用接口断言探测能力（ADR-0003），open 结果里带回主程序。
func supports[T any](src source.Source, _ *T) bool {
	_, ok := src.(T)
	return ok
}

func callErr(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}

// Main 供 cmd/lazydb-driver 薄封装调用（也是测试 re-exec 的入口：
// argv[1] 忽略，连接配置在 open 请求里）。
func Main() {
	if err := Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "driveragent:", err)
		os.Exit(1)
	}
}
