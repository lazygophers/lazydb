// Package conn 管理数据源连接：注册驱动、打开、按 ID 存取。
package conn

import (
	"context"
	"fmt"
	"sync"

	"github.com/lazygophers/lazydb/internal/source"
)

// OpenFunc 按驱动名构造 Source。main 里接真实注册表，测试里可注入。
type OpenFunc func(cfg source.Config) (source.Source, error)

// Manager 持有当前所有连接。v1 为内存态，凭据持久化在 v1-10。
type Manager struct {
	Open OpenFunc

	mu    sync.Mutex
	conns map[string]*Conn
	next  int
}

// Conn 是一条已打开的连接。
type Conn struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Cfg  source.Config  `json:"config"`
	Src  source.Source `json:"-"`
}

func NewManager(open OpenFunc) *Manager {
	return &Manager{Open: open, conns: make(map[string]*Conn)}
}

// Add 打开一条新连接并登记，返回分配的 ID。
func (m *Manager) Add(ctx context.Context, name string, cfg source.Config) (*Conn, error) {
	src, err := m.Open(cfg)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Driver, err)
	}
	if err := src.Open(ctx, cfg); err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Driver, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	id := fmt.Sprintf("%d", m.next)
	c := &Conn{ID: id, Name: name, Cfg: cfg, Src: src}
	m.conns[id] = c
	return c, nil
}

// Get 按 ID 取连接；不存在返回错误。
func (m *Manager) Get(id string) (*Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, fmt.Errorf("connection %q not found", id)
	}
	return c, nil
}

// List 返回全部连接（按创建顺序）。
func (m *Manager) List() []*Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Conn, 0, len(m.conns))
	for i := 1; i <= m.next; i++ {
		if c, ok := m.conns[fmt.Sprintf("%d", i)]; ok {
			out = append(out, c)
		}
	}
	return out
}

// Remove 关闭并删除一条连接。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	c, ok := m.conns[id]
	if ok {
		delete(m.conns, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("connection %q not found", id)
	}
	return c.Src.Close()
}
