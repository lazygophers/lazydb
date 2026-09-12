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
	"strings"

	"github.com/lazygophers/lazydb/drivers/builtin"
	"github.com/lazygophers/lazydb/internal/api"
	"github.com/lazygophers/lazydb/internal/audit"
	"github.com/lazygophers/lazydb/internal/cache"
	"github.com/lazygophers/lazydb/internal/conn"
	"github.com/lazygophers/lazydb/internal/connstore"
	"github.com/lazygophers/lazydb/internal/driverhost"
	"github.com/lazygophers/lazydb/internal/history"
	"github.com/lazygophers/lazydb/internal/mcpserver"
	"github.com/lazygophers/lazydb/internal/runtimefile"
	"github.com/lazygophers/lazydb/internal/secrets"
	"github.com/lazygophers/lazydb/internal/settings"
	"github.com/lazygophers/lazydb/internal/source"
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

	hist, err := history.Open(*home)
	if err != nil {
		return fmt.Errorf("open history.db: %w", err)
	}
	defer hist.Close()
	aud, err := audit.Open(*home)
	if err != nil {
		return fmt.Errorf("open audit.log: %w", err)
	}
	defer aud.Close()

	set, err := settings.Open(*home)
	if err != nil {
		return fmt.Errorf("open settings: %w", err)
	}

	// 驱动插件化（ADR-0002，#33 默认下载）：LAZYDB_PLUGIN_DRIVERS（默认 mysql）
	// 里的驱动走 driverhost（索引→下载→独立进程）。索引 URL 优先级 =
	// 设置 driver_index_url > LAZYDB_DRIVER_INDEX > 默认官方索引；
	// 离线且本地已装驱动时不联网直接用。
	open := builtin.Open
	dh := driverhost.New(*home)
	dh.IndexURL = driverhost.ResolveIndexURL(set.Get().DriverIndexURL, os.Getenv("LAZYDB_DRIVER_INDEX"))
	defer dh.Close()
	plug := map[string]bool{}
	for _, n := range strings.Split(envOr("LAZYDB_PLUGIN_DRIVERS", "mysql"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			plug[n] = true
		}
	}
	builtinOpen := open
	open = func(cfg source.Config) (source.Source, error) {
		if plug[cfg.Driver] {
			return dh.OpenFunc()(cfg)
		}
		return builtinOpen(cfg)
	}

	// 凭据与连接持久化（#25）：DSN 进 OS 钥匙串，磁盘只留元数据。
	// 钥匙串不可用（无头 Linux 等）则连接重启即丢，功能不受影响。
	sec, secErr := secrets.Open()
	if secErr != nil {
		log.Printf("钥匙串不可用（%v）：连接凭据不持久化", secErr)
		sec = nil
	}
	m := conn.NewManager(open)
	if err := connstore.Wire(m, sec, *home); err != nil {
		log.Printf("恢复已存连接失败：%v", err)
	}

	h := api.NewWithSettings(m, store, token, hist, aud, set)
	log.Printf("lazydb sidecar listening on %s", ln.Addr())
	root := http.NewServeMux()
	root.Handle("/", h)
	srv := &http.Server{Handler: root}
	// 完全退出（ADR-0007）：v1 以界面内「退出后端」替代托盘菜单（见 ADR-0007 注）
	root.HandleFunc("POST /api/shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		go srv.Close()
	})
	return srv.Serve(ln)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
