// Package secrets 存连接凭据（#25）：DSN 整体进 OS 钥匙串，
// 磁盘上的 connections.json 只留元数据，无明文凭据。
package secrets

import (
	"errors"
	"runtime"
	"strings"

	"github.com/99designs/keyring"
)

// ErrNotFound 键不存在（连接已被删等）。
var ErrNotFound = errors.New("secrets: not found")

// Store 是凭据存取的最小接口。测试注入假实现，生产用钥匙串。
type Store interface {
	Set(id, dsn string) error
	Get(id string) (string, error)
	Delete(id string) error
	Keys() ([]string, error) // 现存全部 conn id
}

// Open 打开本机钥匙串（macOS Keychain / Windows 凭据管理器 /
// Linux SecretService）。禁用 file 后端——那是明文，违背本包存在的意义；
// 无桌面钥匙串的环境（无头 Linux）返回错误，调用方跳过该连接。
func Open() (Store, error) {
	native := map[string][]keyring.BackendType{
		"darwin":  {keyring.KeychainBackend},
		"windows": {keyring.WinCredBackend},
		"linux":   {keyring.SecretServiceBackend, keyring.KWalletBackend},
	}[runtime.GOOS]
	kr, err := keyring.Open(keyring.Config{
		ServiceName:              "lazydb",
		AllowedBackends:          native,
		KeychainTrustApplication: true,
		WinCredPrefix:            "lazydb/",
	})
	if err != nil {
		return nil, err
	}
	return keyringStore{kr}, nil
}

type keyringStore struct{ kr keyring.Keyring }

func (k keyringStore) Set(id, dsn string) error {
	return k.kr.Set(keyring.Item{Key: "conn-" + id, Data: []byte(dsn)})
}

func (k keyringStore) Get(id string) (string, error) {
	it, err := k.kr.Get("conn-" + id)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(it.Data), nil
}

func (k keyringStore) Delete(id string) error {
	err := k.kr.Remove("conn-" + id)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil // 已不在，删除幂等
	}
	return err
}

func (k keyringStore) Keys() ([]string, error) {
	keys, err := k.kr.Keys()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, key := range keys {
		if id, ok := strings.CutPrefix(key, "conn-"); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
