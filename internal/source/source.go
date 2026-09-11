// Package source 定义数据源抽象：核心接口 + 能力接口（ADR-0003）。
// 本包不含任何具体驱动。
package source

import "context"

// Config 是打开一个数据源所需的全部信息。DSN 格式由驱动自行解释。
type Config struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
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

// Result 是一次 Exec 的返回。Truncated 表示因 MaxRows 截断。
type Result struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated"`
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
