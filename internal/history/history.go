// Package history 查询历史（#23）：每次执行的语句、耗时、成败，
// 落 ~/.lazydb/history.db（WAL，与 cache.db 同款并发策略），可按关键字搜。
package history

import (
	"context"
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// Entry 一条执行记录。
type Entry struct {
	TS   int64  `json:"ts"`   // unix 毫秒
	Conn string `json:"conn"` // 连接 ID
	SQL  string `json:"sql"`
	MS   int64  `json:"ms"` // 耗时
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
}

// Store 历史库。一个进程一个，WAL 支持后端与 MCP 并发写。
type Store struct {
	db *sql.DB
}

func Open(home string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+home+"/history.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS history (
		ts INTEGER NOT NULL, conn TEXT NOT NULL, sql TEXT NOT NULL,
		ms INTEGER NOT NULL, ok INTEGER NOT NULL, error TEXT)`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save 追加一条（只追加，不更新）。
func (s *Store) Save(_ context.Context, e Entry) error {
	_, err := s.db.Exec(`INSERT INTO history (ts, conn, sql, ms, ok, error) VALUES (?,?,?,?,?,?)`,
		e.TS, e.Conn, e.SQL, e.MS, e.OK, e.Err)
	return err
}

// List 按关键字（子串，sql/conn/error 内）倒序取最近 limit 条；q 空取全部。
func (s *Store) List(_ context.Context, q string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT ts, conn, sql, ms, ok, error FROM history
		WHERE ? = '' OR sql LIKE '%' || ? || '%' OR conn LIKE '%' || ? || '%' OR error LIKE '%' || ? || '%'
		ORDER BY ts DESC, rowid DESC LIMIT ?`, q, q, q, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ok int
		if err := rows.Scan(&e.TS, &e.Conn, &e.SQL, &e.MS, &ok, &e.Err); err != nil {
			return nil, err
		}
		e.OK = ok == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune 删掉 cutoff 之前的记录（v1 不接 UI，留给 #25 收尾用）。
func (s *Store) Prune(_ context.Context, cutoff time.Time) error {
	_, err := s.db.Exec(`DELETE FROM history WHERE ts < ?`, cutoff.UnixMilli())
	return err
}
