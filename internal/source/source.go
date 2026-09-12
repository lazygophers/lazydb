// Package source 定义数据源抽象：核心接口 + 能力接口（ADR-0003）。
// 本包不含任何具体驱动。
package source

import (
	"context"
	"strings"
)

// Config 是打开一个数据源所需的全部信息。DSN 格式由驱动自行解释。
type Config struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
	// SSH 非 nil 时经 SSH 隧道连 DSN 里的目标（v1-3，#18）。
	SSH *SSHConfig `json:"ssh,omitempty"`
}

// SSHConfig：经跳板机连数据源。密钥文件路径而非密钥内容（凭据不落输出）。
type SSHConfig struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	KeyPath    string `json:"key_path"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
}

// Path 是结构树上的位置：实例 → 库 → 表 …，逐级下钻。
type Path []string

// Node 是树上的一个子节点。
type Node struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // database | table | view | topic | …
}

// ExecOptions 控制 Exec 的行为。零值 = 默认。
type ExecOptions struct {
	// MaxRows > 0 时最多返回这么多行。
	MaxRows int `json:"max_rows"`
}

// Column 描述表的一个字段。
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

// Index 描述表的一个索引。
type Index struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
}

// Result 是一次 Exec 的返回。Truncated 表示因 MaxRows 截断；
// RowsAffected 是写语句（INSERT/UPDATE/DELETE/DDL）的受影响行数，读语句为 0。
type Result struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	Truncated    bool     `json:"truncated"`
	RowsAffected int64    `json:"rows_affected"`
}

// Source 是每类数据源必须实现的核心接口（ADR-0003）。
type Source interface {
	Open(ctx context.Context, cfg Config) error
	Close() error
	Ping(ctx context.Context) error
	// Children 列结构：空 path 返回顶层（如库），逐级下钻到表为止。
	Children(ctx context.Context, path Path) ([]Node, error)
	Exec(ctx context.Context, stmt string, opts ExecOptions) (Result, error)
}

// 能力接口：谁能谁实现，上层类型断言探测。

type ColumnLister interface {
	Columns(ctx context.Context, table Path) ([]Column, error)
}

type IndexLister interface {
	Indexes(ctx context.Context, table Path) ([]Index, error)
}

type DDLShower interface {
	DDL(ctx context.Context, obj Path) (string, error)
}

// RowStreamer 是导出用能力接口（#24）：逐行回调，不整包进内存。
type RowStreamer interface {
	// Stream 执行语句：列名经 header 先送达（空结果也送），随后逐行回调 row。
	// 任一回调返回错误即中断。
	Stream(ctx context.Context, stmt string, header func(cols []string) error, row func([]any) error) error
}

// ReadOnlyExecer 只读执行能力接口（#30）：语句在事务里执行并永远回滚，
// 白名单挡不住的变体（如 CTE 藏写）由回滚兜底。谁能谁实现。
type ReadOnlyExecer interface {
	ExecReadOnly(ctx context.Context, stmt string, opts ExecOptions) (Result, error)
}

// readVerbs 只读动词白名单。ReadVerb 按首词判断（MCP 默认只读闸门也用它）。
// 只是快速拒绝第一道，真闸门在 ReadOnlyExecer 的事务回滚上。
var readVerbs = map[string]bool{
	"select": true, "show": true, "desc": true, "describe": true,
	"explain": true, "with": true, "pragma": true, "use": true,
}

// FirstVerb 返回语句首词（小写），错误信息用。
func FirstVerb(sql string) string {
	s := strings.TrimSpace(sql)
	if i := strings.IndexAny(s, " \t\r\n;("); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// ReadVerb 报告语句首词是否只读动词。
func ReadVerb(sql string) bool {
	return readVerbs[FirstVerb(sql)]
}
