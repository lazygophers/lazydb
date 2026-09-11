// Package runtimefile 写读 ~/.lazydb/runtime：界面发现 sidecar 的
// 端口 + 令牌（ADR-0001 边界规则）。ADR-0007 的重连协议也读它。
package runtimefile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Info 是 runtime 文件的内容。
type Info struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// Path 返回 runtime 文件路径。home 为用户主目录（测试可注入）。
func Path(home string) string {
	return filepath.Join(home, ".lazydb", "runtime")
}

// Write 落盘（原子：先写临时文件再 rename）。
func Write(home string, info Info) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	dir := filepath.Dir(Path(home))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "runtime-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), Path(home))
}

// Read 读回；文件不存在返回错误。
func Read(home string) (Info, error) {
	var info Info
	b, err := os.ReadFile(Path(home))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return info, fmt.Errorf("runtime file corrupt: %w", err)
	}
	return info, nil
}

// NewToken 生成随机会话令牌（32 hex 字符）。
func NewToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
