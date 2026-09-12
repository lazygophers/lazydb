// Package cache 是 schema 缓存（ADR-0004）：连接过的库/表/字段/索引
// 落进 ~/.lazydb/cache.db 单文件，只存结构不存数据。
// 并发安全靠 WAL + busy_timeout；新取的数据整体覆盖旧行。
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultTTL：fetched_at 距今超过它就算过期，下次访问重拉。
const DefaultTTL = time.Hour

// Store 是 cache.db 的句柄。零值不可用，经 Open 创建。
type Store struct {
	db *sql.DB
}

// Path 返回 cache.db 路径。home 为用户主目录（测试可注入）。
func Path(home string) string {
	return filepath.Join(home, ".lazydb", "cache.db")
}

// Open 打开（必要时创建）cache.db，开启 WAL 与 busy 等待。
func Open(home string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(Path(home)), 0o700); err != nil {
		return nil, err
	}
	// 单写连接：并发靠多进程各自一条连接 + WAL，进程内串行写最稳
	db, err := sql.Open("sqlite", Path(home)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS objects (
		conn       TEXT NOT NULL,
		path       TEXT NOT NULL,
		kind       TEXT NOT NULL,
		payload    TEXT NOT NULL,
		fetched_at INTEGER NOT NULL,
		PRIMARY KEY (conn, path, kind)
	)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Kind：缓存对象的种类。
const (
	KindChildren    = "children"
	KindColumns     = "columns"
	KindIndexes     = "indexes"
	KindDDL         = "ddl"
	KindForeignKeys = "foreign_keys"
)

// Entry 是一条缓存记录。
type Entry struct {
	Payload   []byte
	FetchedAt time.Time
}

// Get 取一条缓存；没有返回 ok=false。
func (s *Store) Get(ctx context.Context, connKey string, path []string, kind string) (Entry, bool, error) {
	var e Entry
	var payload string
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, fetched_at FROM objects WHERE conn=? AND path=? AND kind=?`,
		connKey, joinPath(path), kind).
		Scan(&payload, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, err
	}
	e.Payload, e.FetchedAt = []byte(payload), time.Unix(at, 0)
	return e, true, nil
}

// Save 写一条缓存（同键整体覆盖）。
func (s *Store) Save(ctx context.Context, connKey string, path []string, kind string, payload any, at time.Time) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO objects (conn, path, kind, payload, fetched_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(conn, path, kind) DO UPDATE
		 SET payload=excluded.payload, fetched_at=excluded.fetched_at`,
		connKey, joinPath(path), kind, string(b), at.Unix())
	return err
}

// Drop 删除一条缓存（连着它下面的层级一起，path 前缀匹配）。
func (s *Store) Drop(ctx context.Context, connKey string, path []string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM objects WHERE conn=? AND (path=? OR path LIKE ? ESCAPE '\')`,
		connKey, joinPath(path), likePrefix(path))
	return err
}

// QuickCheck 跑 SQLite 自检，验证库没坏（并发测试用）。
func (s *Store) QuickCheck(ctx context.Context) (string, error) {
	var res string
	err := s.db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&res)
	return res, err
}

func joinPath(path []string) string { return strings.Join(path, "\x1f") }

func likePrefix(path []string) string {
	// 前缀匹配要带上分隔符，避免 "main" 命中 "main2"
	p := strings.Join(append(append([]string{}, path...), ""), "\x1f")
	return strings.ReplaceAll(p, `\`, `\\`) + "%"
}
