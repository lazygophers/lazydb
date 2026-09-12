// Package settings：~/.lazydb/settings.json（0600，无凭据）。
// #29：「保留核心」开关；driver_index_url 为 #33 占位（空 = 默认）。
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Settings struct {
	KeepCoreOnClose bool   `json:"keep_core_on_close"` // 界面关闭后保留后端核心（ADR-0007，默认开）
	DriverIndexURL  string `json:"driver_index_url"`   // 空 = 内置默认索引
}

// Default：ADR-0007 决策 = 保留核心默认开（不改默认的用户在设置里关）。
func Default() Settings { return Settings{KeepCoreOnClose: true} }

type Store struct {
	mu   sync.Mutex
	path string
	s    Settings
}

// Open 读 home/.lazydb/settings.json；缺文件/损坏文件都回默认值（设置不该挡启动）。
func Open(home string) (*Store, error) {
	s := Default()
	path := filepath.Join(home, ".lazydb", "settings.json")
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s) // 损坏 JSON：Unmarshal 报错但已解出的字段保留，够用
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return &Store{path: path, s: s}, nil
}

func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.s
}

func (s *Store) Save(n Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(s.path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	s.s = n
	return nil
}
