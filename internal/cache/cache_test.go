package cache_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/source"
)

func TestSaveGetDrop(t *testing.T) {
	dir := t.TempDir()
	s, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	nodes := []source.Node{{Name: "users", Kind: "table"}}
	if err := s.Save(ctx, "k1", []string{"main"}, cache.KindChildren, nodes, time.Now()); err != nil {
		t.Fatal(err)
	}

	e, ok, err := s.Get(ctx, "k1", []string{"main"}, cache.KindChildren)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	var got []source.Node
	if err := jsonUnmarshal(e.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "users" {
		t.Fatalf("payload = %+v", got)
	}
	if time.Since(e.FetchedAt) > time.Minute {
		t.Fatalf("fetchedAt = %v", e.FetchedAt)
	}

	// 其它 key 不串
	if _, ok, _ := s.Get(ctx, "k2", []string{"main"}, cache.KindChildren); ok {
		t.Fatal("wrong key hit")
	}

	// Drop 连带子层级：["main"] 删掉 ["main"] 与 ["main","t"]
	if err := s.Save(ctx, "k1", []string{"main", "users"}, cache.KindColumns, []source.Column{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(ctx, "k1", []string{"main"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "k1", []string{"main"}, cache.KindChildren); ok {
		t.Fatal("drop left parent row")
	}
	if _, ok, _ := s.Get(ctx, "k1", []string{"main", "users"}, cache.KindColumns); ok {
		t.Fatal("drop left child row")
	}
}

// 重开（模拟后端重启）数据仍在。
func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), "k", []string{"main"}, cache.KindDDL, "CREATE TABLE x", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	e, ok, err := s2.Get(context.Background(), "k", []string{"main"}, cache.KindDDL)
	if err != nil || !ok || string(e.Payload) != `"CREATE TABLE x"` {
		t.Fatalf("reopen: ok=%v payload=%s err=%v", ok, e.Payload, err)
	}
}

// 两进程并发读写（各一条连接 + WAL）：不损坏（quick_check = ok）。
func TestConcurrentTwoStoresNoCorruption(t *testing.T) {
	dir := t.TempDir()
	a, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := cache.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			st := [2]*cache.Store{a, b}[g%2]
			for i := 0; i < 50; i++ {
				nodes := []source.Node{{Name: "t", Kind: "table"}}
				_ = st.Save(ctx, "conn", []string{"db"}, cache.KindChildren, nodes, time.Now())
				_, _, _ = st.Get(ctx, "conn", []string{"db"}, cache.KindChildren)
			}
		}(g)
	}
	wg.Wait()

	for _, st := range []*cache.Store{a, b} {
		res, err := st.QuickCheck(ctx)
		if err != nil || res != "ok" {
			t.Fatalf("quick_check = %q err=%v", res, err)
		}
	}
}

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
