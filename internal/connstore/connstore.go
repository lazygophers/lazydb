// Package connstore 连接持久化（#25）：元数据落 connections.json（0600），
// DSN 整体进 secrets.Store（OS 钥匙串），磁盘无明文凭据。
package connstore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/secrets"
	"github.com/lazygophers/lazydb/internal/source"
)

// Entry connections.json 里的一条。
type Entry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

// Wire 装上持久化：先恢复已存连接，再在 Manager 增删后自动落盘。
// st 为 nil 时只落元数据不落 DSN（测试用）；恢复失败的单条跳过不拦启动。
func Wire(m *conn.Manager, st secrets.Store, home string) error {
	entries, _ := load(home)
	for _, e := range entries {
		if st == nil {
			continue // 没有凭据源，无从恢复（连接可能带密码）
		}
		dsn, err := st.Get(e.ID)
		if err != nil {
			log.Printf("连接 %q 的凭据取不到（%v），跳过恢复", e.Name, err)
			continue
		}
		if _, err := m.Add(context.Background(), e.Name, source.Config{Driver: e.Driver, DSN: dsn}); err != nil {
			log.Printf("连接 %q 恢复失败（%v），跳过", e.Name, err)
		}
	}
	m.Persist = func() { persist(m, st, home) }
	persist(m, st, home) // ID 恢复后可能重排，立即重写一次
	return nil
}

func persist(m *conn.Manager, st secrets.Store, home string) {
	conns := m.List()
	entries := make([]Entry, len(conns))
	live := map[string]bool{}
	for i, c := range conns {
		entries[i] = Entry{ID: c.ID, Name: c.Name, Driver: c.Cfg.Driver}
		live[c.ID] = true
		if st != nil {
			if err := st.Set(c.ID, c.Cfg.DSN); err != nil {
				log.Printf("凭据入库失败（连接 %s）：%v", c.ID, err)
			}
		}
	}
	// 删掉的连接顺手清掉钥匙串里的孤儿凭据
	if st != nil {
		if keys, err := st.Keys(); err == nil {
			for _, id := range keys {
				if !live[id] {
					_ = st.Delete(id)
				}
			}
		}
	}
	b, _ := json.MarshalIndent(entries, "", "  ")
	path := filepath.Join(home, ".lazydb")
	os.MkdirAll(path, 0o700)
	if err := os.WriteFile(filepath.Join(path, "connections.json"), b, 0o600); err != nil {
		log.Printf("connections.json 写入失败：%v", err)
	}
}

func load(home string) ([]Entry, error) {
	b, err := os.ReadFile(filepath.Join(home, ".lazydb", "connections.json"))
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("connections.json: %w", err)
	}
	return entries, nil
}
