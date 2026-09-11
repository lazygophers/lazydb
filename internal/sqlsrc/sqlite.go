// Package sqlsrc 是 SQLite 测试数据源（spec #15 测试缝用的数据源，
// 不算「支持 SQLite 数据源」）。实现 Source 全部核心接口与三个能力接口。
package sqlsrc

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lazygophers/lazydb/internal/source"

	_ "modernc.org/sqlite"
)

// SQLite 是基于 modernc.org/sqlite（纯 Go，CGO_ENABLED=0 可编）的测试源。
type SQLite struct {
	db *sql.DB
}

var _ source.ColumnLister = (*SQLite)(nil)
var _ source.IndexLister = (*SQLite)(nil)
var _ source.DDLShower = (*SQLite)(nil)

func (s *SQLite) Open(_ context.Context, cfg source.Config) error {
	if cfg.Driver != "sqlite" {
		return fmt.Errorf("sqlsrc: driver %q != sqlite", cfg.Driver)
	}
	db, err := sql.Open("sqlite", cfg.DSN)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // sqlite 单写者，串行化最简单也最快到不出错
	s.db = db
	return nil
}

func (s *SQLite) Close() error { return s.db.Close() }

func (s *SQLite) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Children：空 path → 数据库列表（SQLite 单文件就是 main）；
// ["main"] → 表和视图（排除内部表 sqlite_%）；更深返回空。
func (s *SQLite) Children(ctx context.Context, path source.Path) ([]source.Node, error) {
	switch len(path) {
	case 0:
		return []source.Node{{Name: "main", Kind: "database"}}, nil
	case 1:
		if path[0] != "main" {
			return nil, fmt.Errorf("sqlsrc: unknown database %q", path[0])
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT name, type FROM sqlite_master
			 WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
			 ORDER BY name`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []source.Node
		for rows.Next() {
			var n source.Node
			if err := rows.Scan(&n.Name, &n.Kind); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	default:
		return nil, nil // 字段走 Columns 能力接口，不下钻
	}
}

func (s *SQLite) Exec(ctx context.Context, stmt string, opts source.ExecOptions) (source.Result, error) {
	if !source.ReadVerb(stmt) {
		// 写语句走 Exec 拿受影响行数（#24）。
		// ponytail: 首词判断，INSERT…RETURNING 拿不到返回集，需要时再加。
		r, err := s.db.ExecContext(ctx, stmt)
		if err != nil {
			return source.Result{}, err
		}
		n, _ := r.RowsAffected()
		return source.Result{RowsAffected: n}, nil
	}
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return source.Result{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return source.Result{}, err
	}
	if len(cols) == 0 {
		return source.Result{}, nil
	}
	res := source.Result{Columns: cols}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return source.Result{}, err
		}
		res.Rows = append(res.Rows, vals)
		if opts.MaxRows > 0 && len(res.Rows) == opts.MaxRows {
			res.Truncated = true
			break
		}
	}
	return res, rows.Err()
}

var _ source.RowStreamer = (*SQLite)(nil)

// Stream 逐行回调（#24 导出缝）：database/sql 本身游标式，天然流式。
func (s *SQLite) Stream(ctx context.Context, stmt string, header func([]string) error, emit func(row []any) error) error {
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("sqlsrc: 非查询语句，无结果可导出")
	}
	if err := header(cols); err != nil {
		return err
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if err := emit(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Columns 列出一张表的字段。table 形如 ["main", "表名"]，末位是表名。
func (s *SQLite) Columns(ctx context.Context, table source.Path) ([]source.Column, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf("PRAGMA table_info(%s)", quoteIdent(last(table))))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Column
	for rows.Next() {
		var c source.Column
		var cid, notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &c.Name, &c.Type, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		c.Nullable = notnull == 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// Indexes 列出一张表的索引。
func (s *SQLite) Indexes(ctx context.Context, table source.Path) ([]source.Index, error) {
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf("PRAGMA index_list(%s)", quoteIdent(last(table))))
	if err != nil {
		return nil, err
	}
	// 单连接下不能嵌套查询：先收完外层，再逐个查索引列
	type rawIndex struct {
		ix     source.Index
		unique bool
	}
	var raws []rawIndex
	for rows.Next() {
		var ix source.Index
		var seq, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &ix.Name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		raws = append(raws, rawIndex{ix: ix, unique: unique == 1})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]source.Index, 0, len(raws))
	for _, r := range raws {
		cols, err := s.indexColumns(ctx, r.ix.Name)
		if err != nil {
			return nil, err
		}
		r.ix.Columns, r.ix.Unique = cols, r.unique
		out = append(out, r.ix)
	}
	return out, nil
}

func (s *SQLite) indexColumns(ctx context.Context, index string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%s)", quoteIdent(index)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var seqno, cid int
		var name sql.NullString
		if err := rows.Scan(&seqno, &cid, &name); err != nil {
			return nil, err
		}
		out = append(out, name.String)
	}
	return out, rows.Err()
}

// DDL 返回一个对象（表/视图/索引）的建表语句。
func (s *SQLite) DDL(ctx context.Context, obj source.Path) (string, error) {
	var ddl sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE name = ? AND sql IS NOT NULL`, last(obj)).
		Scan(&ddl)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("sqlsrc: no DDL for %q", last(obj))
	}
	if err != nil {
		return "", err
	}
	return ddl.String, nil
}

func last(p source.Path) string { return p[len(p)-1] }

// quoteIdent 用双引号包住标识符，内部双引号翻倍（SQLite 转义规则）。
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
