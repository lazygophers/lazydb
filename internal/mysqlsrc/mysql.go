// Package mysqlsrc 是内置 MySQL 数据源（v1-3 tracer bullet，#18）。
// 实现 Source 全部核心接口与三个能力接口，走 information_schema 元数据。
package mysqlsrc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/go-sql-driver/mysql"
	"github.com/lazygophers/lazydb/internal/source"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// MySQL 数据源。SSH 隧道时持有 ssh client，Close 一并关掉。
type MySQL struct {
	db        *sql.DB
	tunnel    *ssh.Client
	dialProto string // 注册进 mysql driver 的伪协议名，Close 时注销
}

var _ source.ColumnLister = (*MySQL)(nil)
var _ source.IndexLister = (*MySQL)(nil)
var _ source.DDLShower = (*MySQL)(nil)
var _ source.RowStreamer = (*MySQL)(nil)
var _ source.ReadOnlyExecer = (*MySQL)(nil)
var _ source.ForeignKeyLister = (*MySQL)(nil)

var dialSeq atomic.Int64

func (m *MySQL) Open(_ context.Context, cfg source.Config) error {
	if cfg.Driver != "mysql" {
		return fmt.Errorf("mysqlsrc: driver %q != mysql", cfg.Driver)
	}
	dsn, err := mysql.ParseDSN(cfg.DSN)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	if sc := cfg.SSH; sc != nil {
		client, err := dialSSH(sc)
		if err != nil {
			return fmt.Errorf("ssh dial %s:%d: %w", sc.Host, sc.Port, err)
		}
		m.tunnel = client
		m.dialProto = fmt.Sprintf("lazydb-ssh-%d", dialSeq.Add(1))
		mysql.RegisterDialContext(m.dialProto, func(ctx context.Context, addr string) (net.Conn, error) {
			return client.DialContext(ctx, "tcp", addr)
		})
		dsn.Net = m.dialProto
		dsn.Addr = net.JoinHostPort(sc.TargetHost, fmt.Sprint(sc.TargetPort))
	}
	db, err := sql.Open("mysql", dsn.FormatDSN())
	if err != nil {
		m.Close()
		return err
	}
	m.db = db
	return nil
}

func (m *MySQL) Close() error {
	var firstErr error
	if m.db != nil {
		firstErr = m.db.Close()
	}
	if m.dialProto != "" {
		mysql.DeregisterDialContext(m.dialProto)
	}
	if m.tunnel != nil {
		if err := m.tunnel.Close(); firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *MySQL) Ping(ctx context.Context) error {
	var one int
	return m.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Children：空 path → 库列表；[db] → 表/视图。
func (m *MySQL) Children(ctx context.Context, path source.Path) ([]source.Node, error) {
	switch len(path) {
	case 0:
		rows, err := m.db.QueryContext(ctx, "SHOW DATABASES")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []source.Node
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			out = append(out, source.Node{Name: name, Kind: "database"})
		}
		return out, rows.Err()
	case 1:
		rows, err := m.db.QueryContext(ctx,
			`SELECT table_name, table_type FROM information_schema.tables
			 WHERE table_schema = ? ORDER BY table_name`, path[0])
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []source.Node
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				return nil, err
			}
			kind := "table"
			if typ == "VIEW" {
				kind = "view"
			}
			out = append(out, source.Node{Name: name, Kind: kind})
		}
		return out, rows.Err()
	default:
		return nil, nil // 字段走 Columns 能力接口
	}
}

func (m *MySQL) Exec(ctx context.Context, stmt string, opts source.ExecOptions) (source.Result, error) {
	if !source.ReadVerb(stmt) {
		// 写语句走 Exec 拿受影响行数（#24）。
		// ponytail: 首词判断，写语句带返回集时拿不到，需要时再加。
		r, err := m.db.ExecContext(ctx, stmt)
		if err != nil {
			return source.Result{}, err
		}
		n, _ := r.RowsAffected()
		return source.Result{RowsAffected: n}, nil
	}
	rows, err := m.db.QueryContext(ctx, stmt)
	if err != nil {
		return source.Result{}, err
	}
	defer rows.Close()
	return collectRows(rows, opts)
}

// ForeignKeys（#34）：information_schema 双表 JOIN，按约束名分组合成。
func (m *MySQL) ForeignKeys(ctx context.Context, table source.Path) ([]source.ForeignKey, error) {
	db, tbl := table[0], table[len(table)-1]
	rows, err := m.db.QueryContext(ctx, `
		SELECT kcu.constraint_name, kcu.column_name,
		       kcu.referenced_table_name, kcu.referenced_column_name,
		       rc.update_rule, rc.delete_rule
		FROM information_schema.key_column_usage kcu
		JOIN information_schema.referential_constraints rc
		  ON kcu.constraint_schema = rc.constraint_schema
		 AND kcu.constraint_name  = rc.constraint_name
		WHERE kcu.table_schema = ? AND kcu.table_name = ?
		ORDER BY kcu.constraint_name, kcu.ordinal_position`, db, tbl)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []source.ForeignKey{}
	for rows.Next() {
		var name, col, refTbl, refCol, onUpdate, onDelete string
		if err := rows.Scan(&name, &col, &refTbl, &refCol, &onUpdate, &onDelete); err != nil {
			return nil, err
		}
		var fk *source.ForeignKey
		for i := range out {
			if out[i].Name == name {
				fk = &out[i]
				break
			}
		}
		if fk == nil {
			out = append(out, source.ForeignKey{
				Name: name, RefTable: refTbl, OnUpdate: onUpdate, OnDelete: onDelete,
			})
			fk = &out[len(out)-1]
		}
		fk.Columns = append(fk.Columns, col)
		fk.RefColumns = append(fk.RefColumns, refCol)
	}
	return out, rows.Err()
}

// ExecReadOnly（#30 只读执行）：语句在事务里跑并永远回滚，
// 白名单挡不住的变体（如 WITH … DELETE）由回滚兜底。
// 注意 MySQL DDL 会隐式提交绕过回滚——白名单首词拒绝仍是第一道，
// DDL 动词不进白名单，走不到这里。
func (m *MySQL) ExecReadOnly(ctx context.Context, stmt string, opts source.ExecOptions) (source.Result, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return source.Result{}, err
	}
	defer tx.Rollback() //nolint:errcheck // 只读路径：回滚失败连接本身已不可信
	rows, err := tx.QueryContext(ctx, stmt)
	if err != nil {
		return source.Result{}, err
	}
	defer rows.Close()
	return collectRows(rows, opts)
}

// collectRows 读出结果集（MaxRows 截断；[]byte 转 string）。共享 Exec/ExecReadOnly。
func collectRows(rows *sql.Rows, opts source.ExecOptions) (source.Result, error) {
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
		for i, v := range vals {
			if b, ok := v.([]byte); ok { // go-sql-driver 的字符串都是 []byte，JSON 会变 base64
				vals[i] = string(b)
			}
		}
		res.Rows = append(res.Rows, vals)
		if opts.MaxRows > 0 && len(res.Rows) == opts.MaxRows {
			res.Truncated = true
			break
		}
	}
	return res, rows.Err()
}

// Stream 逐行回调（#24 导出缝）：database/sql 游标式，天然流式。
func (m *MySQL) Stream(ctx context.Context, stmt string, header func([]string) error, emit func(row []any) error) error {
	rows, err := m.db.QueryContext(ctx, stmt)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf("mysqlsrc: 非查询语句，无结果可导出")
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
		for i, v := range vals {
			if b, ok := v.([]byte); ok { // go-sql-driver 的字符串都是 []byte
				vals[i] = string(b)
			}
		}
		if err := emit(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *MySQL) Columns(ctx context.Context, table source.Path) ([]source.Column, error) {
	db, tbl := table[0], table[len(table)-1]
	rows, err := m.db.QueryContext(ctx,
		`SELECT column_name, data_type, is_nullable FROM information_schema.columns
		 WHERE table_schema = ? AND table_name = ? ORDER BY ordinal_position`, db, tbl)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Column
	for rows.Next() {
		var c source.Column
		var nullable string
		if err := rows.Scan(&c.Name, &c.Type, &nullable); err != nil {
			return nil, err
		}
		c.Nullable = nullable == "YES"
		out = append(out, c)
	}
	return out, rows.Err()
}

// Indexes 用 information_schema.statistics，一次收齐再按索引名分组
// （单连接下避免嵌套查询死锁，同 sqlsrc 的教训）。
func (m *MySQL) Indexes(ctx context.Context, table source.Path) ([]source.Index, error) {
	db, tbl := table[0], table[len(table)-1]
	rows, err := m.db.QueryContext(ctx,
		`SELECT index_name, column_name, non_unique FROM information_schema.statistics
		 WHERE table_schema = ? AND table_name = ? ORDER BY index_name, seq_in_index`, db, tbl)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source.Index
	for rows.Next() {
		var name, col string
		var nonUnique int
		if err := rows.Scan(&name, &col, &nonUnique); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Name != name {
			out = append(out, source.Index{Name: name, Unique: nonUnique == 0})
		}
		ix := &out[len(out)-1]
		ix.Columns = append(ix.Columns, col)
	}
	return out, rows.Err()
}

// DDL：优先 SHOW CREATE TABLE，视图回退 SHOW CREATE VIEW。
func (m *MySQL) DDL(ctx context.Context, obj source.Path) (string, error) {
	db, name := obj[0], obj[len(obj)-1]
	var tbl, ddl string
	q := fmt.Sprintf("SHOW CREATE TABLE %s.%s", quoteIdent(db), quoteIdent(name))
	err := m.db.QueryRowContext(ctx, q).Scan(&tbl, &ddl)
	if err != nil {
		var v1, v2, v3, v4, v5 string // SHOW CREATE VIEW 列多，多余列丢弃
		q2 := fmt.Sprintf("SHOW CREATE VIEW %s.%s", quoteIdent(db), quoteIdent(name))
		if err2 := m.db.QueryRowContext(ctx, q2).Scan(&v1, &v2, &v3, &v4, &v5); err2 != nil {
			return "", fmt.Errorf("no DDL for %s.%s: %w", db, name, err)
		}
		return v2, nil
	}
	return ddl, nil
}

func dialSSH(sc *source.SSHConfig) (*ssh.Client, error) {
	key, err := os.ReadFile(sc.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	cb, err := hostKeyCallback(sc)
	if err != nil {
		return nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            sc.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: cb,
	}
	if sc.HostKeySHA256 != "" {
		// 钉死算法：指纹格式（SHA256:…）不含算法名，而 ssh 客户端默认
		// ECDSA 排 ED25519 前，握手拿到的可能不是指纹对应那把 key
		// （x/crypto ssh/common.go defaultHostKeyAlgos）。
		// ponytail: 只钉 ED25519；要按 ECDSA 指纹校验时加算法声明字段
		cfg.HostKeyAlgorithms = []string{ssh.KeyAlgoED25519}
	}
	return ssh.Dial("tcp", net.JoinHostPort(sc.Host, fmt.Sprint(sc.Port)), cfg)
}

// hostKeyCallback 主机指纹校验（#31）：配置显式指纹优先于 known_hosts；
// 两个来源都没有 = 拒连，不静默放行。
func hostKeyCallback(sc *source.SSHConfig) (ssh.HostKeyCallback, error) {
	addr := net.JoinHostPort(sc.Host, fmt.Sprint(sc.Port))
	if sc.HostKeySHA256 != "" {
		want, err := parseFingerprint(sc.HostKeySHA256)
		if err != nil {
			return nil, fmt.Errorf("host_key_sha256 格式错误（应为 SHA256:… 同 ssh-keygen -lf）：%w", err)
		}
		return func(_ string, _ net.Addr, key ssh.PublicKey) error {
			got := sha256.Sum256(key.Marshal())
			if subtle.ConstantTimeCompare(got[:], want) == 1 {
				return nil
			}
			return fmt.Errorf("跳板机 %s 主机指纹不匹配：期望 SHA256:%s，实际 SHA256:%s。跳板机确实换过密钥才更新指纹，否则可能是中间人",
				addr, fpBase64(want), fpBase64(got[:]))
		}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("跳板机 %s 无法校验主机指纹：取不到主目录（%v）", addr, err)
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	kh, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("跳板机 %s 无法校验主机指纹：未配置 host_key_sha256 且读不了 %s（%v）。可执行 ssh-keyscan -p %d %s >> %s，或在连接配置里填 host_key_sha256",
			addr, path, err, sc.Port, sc.Host, path)
	}
	return func(hostname string, a net.Addr, key ssh.PublicKey) error {
		if err := kh(hostname, a, key); err != nil {
			return fmt.Errorf("跳板机 %s 主机指纹校验失败：%v。跳板机确实换过密钥才从 %s 删旧条目重扫，否则可能是中间人",
				addr, err, path)
		}
		return nil
	}, nil
}

// parseFingerprint 解析 SHA256:xxx（前缀可省）为 32 字节。
func parseFingerprint(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "SHA256:")
	b, err := base64.StdEncoding.WithPadding(base64.NoPadding).DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != sha256.Size {
		return nil, fmt.Errorf("长度 %d 不是 SHA256", len(b))
	}
	return b, nil
}

// fpBase64 编成 OpenSSH 同款指纹串（SHA256: 后 base64 无填充）。
func fpBase64(b []byte) string {
	return base64.StdEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
