package runtimefile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lazygophers/lazydb/internal/runtimefile"
)

func TestWriteReadRoundTrip(t *testing.T) {
	home := t.TempDir()
	info := runtimefile.Info{Port: 54321, Token: "abcd1234", PID: os.Getpid()}
	if err := runtimefile.Write(home, info); err != nil {
		t.Fatal(err)
	}
	got, err := runtimefile.Read(home)
	if err != nil {
		t.Fatal(err)
	}
	if got != info {
		t.Fatalf("round trip: got %+v want %+v", got, info)
	}

	// 覆写：第二个后端起来时替换旧文件
	info2 := runtimefile.Info{Port: 9999, Token: "zz", PID: 1}
	if err := runtimefile.Write(home, info2); err != nil {
		t.Fatal(err)
	}
	if got, err = runtimefile.Read(home); err != nil || got != info2 {
		t.Fatalf("overwrite: got %+v err %v", got, err)
	}
}

func TestReadMissing(t *testing.T) {
	if _, err := runtimefile.Read(t.TempDir()); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestNewTokenUnique(t *testing.T) {
	a, err := runtimefile.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := runtimefile.NewToken()
	if a == b || len(a) != 32 {
		t.Fatalf("tokens not unique/32hex: %q %q", a, b)
	}
}

func TestPathLayout(t *testing.T) {
	if got, want := runtimefile.Path("/home/u"), filepath.Join("/home/u", ".lazydb", "runtime"); got != want {
		t.Fatalf("path = %q want %q", got, want)
	}
}
