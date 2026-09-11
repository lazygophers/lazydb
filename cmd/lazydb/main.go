// lazydb：一个二进制两种模式（ADR-0001）。
// 默认 = sidecar 后端（本机 HTTP+JSON）；`lazydb mcp-server` = MCP 模式（#19）。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/runtimefile"
	"github.com/lazygophers/lazydb/internal/source"
	"github.com/lazygophers/lazydb/internal/sqlsrc"
)

// openSource 按驱动名构造 Source。驱动增多（#18 MySQL）时在此登记。
func openSource(cfg source.Config) (source.Source, error) {
	switch cfg.Driver {
	case "sqlite":
		return &sqlsrc.SQLite{}, nil
	}
	return nil, fmt.Errorf("unknown driver %q", cfg.Driver)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp-server" {
		// v1-4（#19）实现；先占住子命令，避免当普通参数吞掉
		fmt.Fprintln(os.Stderr, "mcp-server 模式尚未实现（#19）")
		os.Exit(2)
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

	h := api.New(conn.NewManager(openSource), token)
	log.Printf("lazydb sidecar listening on %s", ln.Addr())
	srv := &http.Server{Handler: h}
	return srv.Serve(ln)
}
