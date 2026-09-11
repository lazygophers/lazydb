// lazydb：一个二进制两种模式（ADR-0001）。
// 默认 = sidecar 后端（本机 HTTP+JSON）；`lazydb mcp-server` = MCP 模式（#19）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/lazygophers/lazydb/drivers/builtin"
	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/mcpserver"
	"github.com/lazygophers/lazydb/internal/runtimefile"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp-server" {
		fs := flag.NewFlagSet("mcp-server", flag.ExitOnError)
		home := fs.String("home", "", "用户主目录（默认 $HOME）")
		allowWrite := fs.Bool("allow-write", false, "放行写语句（默认只读）")
		_ = fs.Parse(os.Args[2:])
		h := *home
		if h == "" {
			var err error
			if h, err = os.UserHomeDir(); err != nil {
				log.Fatal(err)
			}
		}
		if err := mcpserver.Run(context.Background(), h, *allowWrite); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	home := flag.String("home", "", "用户主目录（默认 $HOME，测试用）")
	addr := flag.String("addr", "127.0.0.1:0", "监听地址（默认随机端口）")
	flag.Parse()

	token, err := runtimefile.NewToken()
	if err != nil {
		return fmt.Errorf("gen token: %w", err)
	}
	if *home == "" {
		*home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	if err := runtimefile.Write(*home, runtimefile.Info{
		Port:  ln.Addr().(*net.TCPAddr).Port,
		Token: token,
		PID:   os.Getpid(),
	}); err != nil {
		return fmt.Errorf("write runtime file: %w", err)
	}

	store, err := cache.Open(*home)
	if err != nil {
		return fmt.Errorf("open cache.db: %w", err)
	}
	defer store.Close()

	h := api.New(conn.NewManager(builtin.Open), store, token)
	log.Printf("lazydb sidecar listening on %s", ln.Addr())
	srv := &http.Server{Handler: h}
	return srv.Serve(ln)
}
