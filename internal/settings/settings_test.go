package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsWhenFileMissing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get()
	if !got.KeepCoreOnClose {
		t.Errorf("默认 keep_core_on_close 应为 true，得到 false")
	}
	if got.DriverIndexURL != "" {
		t.Errorf("默认 driver_index_url 应为空，得到 %q", got.DriverIndexURL)
	}
}

func TestSaveRoundtripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Settings{KeepCoreOnClose: false, DriverIndexURL: "http://mirror"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, ".lazydb", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("settings.json 权限应 0600，得到 %v", fi.Mode().Perm())
	}
	// 重开 = 新进程：读到持久化值
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get().KeepCoreOnClose {
		t.Error("keep_core_on_close 持久化失败")
	}
	if s2.Get().DriverIndexURL != "http://mirror" {
		t.Errorf("driver_index_url 持久化失败：%q", s2.Get().DriverIndexURL)
	}
}

func TestCorruptFileFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".lazydb"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".lazydb", "settings.json"), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("损坏文件应回默认值而不是报错：%v", err)
	}
	if !s.Get().KeepCoreOnClose {
		t.Error("损坏文件后默认值应为 true")
	}
}
