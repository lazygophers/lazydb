// Package audit 审计日志（#23）：界面与 MCP 两条路径的每次执行都追加
// JSON 行到 ~/.lazydb/audit.log（append-only，留痕不可改写语义）。
package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/lazygophers/lazydb/internal/history"
)

// Logger 审计写入器。os.File 是 append 模式，多进程并发 append 行原子。
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// Source 取值："ui"（HTTP 后端）或 "mcp"。
type Entry = struct {
	history.Entry
	Source string `json:"source"`
}

func Open(home string) (*Logger, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(home, "audit.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

func (l *Logger) Close() error { return l.f.Close() }

// Log 追加一条。写失败返回错误（调用方记日志即可，不拦执行主流程的回包）。
func (l *Logger) Log(source string, e history.Entry) error {
	b, err := json.Marshal(Entry{Entry: e, Source: source})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.f.Write(append(b, '\n'))
	return err
}
