// Package driverhost 管驱动代理进程的完整生命周期（ADR-0002）：
// 索引解析 → 按需下载（哈希校验）→ 拉起 → stdio JSON 调用 →
// 空闲回收 / 后端退出时全量回收。僵尸进程是 GoNavi 的实际缺陷，这里不重演。
package driverhost

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/lazygophers/lazydb/internal/source"
)

// ProtocolVersion 是主程序侧的驱动协议版本（须落在索引声明的区间内）。
const ProtocolVersion = 1

// DefaultIdleTimeout 驱动进程无请求多久后自动退出。
const DefaultIdleTimeout = 5 * time.Minute

// Index 是远端驱动索引 JSON 的形状。
type Index struct {
	Drivers map[string]Driver `json:"drivers"`
}

// Driver 单个驱动的声明。Protocol = [min, max]（含两端）。
type Driver struct {
	Version    string               `json:"version"`
	Protocol   [2]int               `json:"protocol"`
	Platforms  map[string]Artifact  `json:"platforms"`
}

// Artifact 一个平台产物的下载地址与 sha256。
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Host 一组驱动进程的宿主。Close 回收全部子进程。
type Host struct {
	Home        string        // ~/.lazydb，驱动落 Home/drivers/
	IndexURL    string        // 远端索引；空则只认本地已装驱动
	IdleTimeout time.Duration // 0 = DefaultIdleTimeout
	HTTP        *http.Client
	// Spawn 供测试替换拉起方式；nil = exec.Command。
	Spawn func(path string, args ...string) *exec.Cmd

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

func New(home string) *Host {
	return &Host{Home: home, clients: map[*client]struct{}{}}
}

// Open 经驱动代理打开数据源：解析索引、按需下载校验、拉起、open 握手。
// name = 驱动名（如 "mysql"）。返回的 source.Source 是代理的本地门面，
// Close 即结束该代理进程。
func (h *Host) Open(ctx context.Context, name string, cfg source.Config) (source.Source, error) {	path, err := h.resolve(ctx, name)
	if err != nil {
		return nil, err
	}
	spawn := h.Spawn
	if spawn == nil {
		spawn = exec.Command
	}
	cmd := spawn(path, name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start driver %s: %w", name, err)
	}

	c := &client{
		h:     h,
		cmd:   cmd,
		stdin: stdin,
		sc:    bufio.NewScanner(stdout),
		scErr: make(chan error, 1),
	}
	c.sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	go func() { c.scErr <- c.sc.Err() }()

	if _, err := c.open(cfg); err != nil {
		c.kill()
		return nil, err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.kill()
		return nil, fmt.Errorf("driverhost closed")
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go h.reapWhenIdle(c)
	// ponytail: 门面恒实现三个能力接口，不支持的在调用点报错——
	// api 侧 501 区分只对内置驱动精确，插件驱动走到调用才见分晓。
	return c, nil
}

// Close 回收全部驱动进程。幂等。
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		c.kill()
		delete(h.clients, c)
	}
	return nil
}

func (h *Host) reapWhenIdle(c *client) {
	timeout := h.IdleTimeout
	if timeout == 0 {
		timeout = DefaultIdleTimeout
	}
	for {
		time.Sleep(timeout / 10)
		if c.exited() {
			h.forget(c)
			return
		}
		if time.Since(c.lastUsed()) > timeout {
			c.kill()
			h.forget(c)
			return
		}
	}
}

func (h *Host) forget(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// ---- 索引与下载 ----

func (h *Host) driverDir() string { return filepath.Join(h.Home, "drivers") }

// resolve 返回驱动二进制路径：本地已装（版本/哈希与索引一致）直接用，
// 否则按索引下载并校验。
func (h *Host) resolve(ctx context.Context, name string) (string, error) {
	idx, err := h.fetchIndex(ctx)
	var drv Driver
	var known bool
	if err == nil {
		drv, known = idx.Drivers[name]
	} else if h.IndexURL != "" {
		return "", fmt.Errorf("fetch driver index: %w", err)
	}
	key := runtime.GOOS + "/" + runtime.GOARCH
	dest := filepath.Join(h.driverDir(), name)

	if known {
		if drv.Protocol[0] > ProtocolVersion || drv.Protocol[1] < ProtocolVersion {
			return "", fmt.Errorf("驱动 %s@%s 协议版本 [%d,%d] 与主程序 %d 不兼容",
				name, drv.Version, drv.Protocol[0], drv.Protocol[1], ProtocolVersion)
		}
		art, ok := drv.Platforms[key]
		if !ok {
			return "", fmt.Errorf("驱动 %s@%s 无 %s 产物", name, drv.Version, key)
		}
		if sum, err := fileSHA256(dest); err == nil && sum == art.SHA256 {
			return dest, nil // 已装且哈希一致
		}
		if err := h.download(ctx, art, dest); err != nil {
			return "", err
		}
		return dest, nil
	}

	// 索引没这个驱动：本地有就用（离线/内置场景），没有才报错
	if st, err := os.Stat(dest); err == nil && !st.IsDir() {
		return dest, nil
	}
	return "", fmt.Errorf("驱动 %s 不在索引中且本地未安装（%v）", name, err)
}

func (h *Host) fetchIndex(ctx context.Context) (*Index, error) {
	if h.IndexURL == "" {
		return nil, fmt.Errorf("no index url")
	}
	hc := h.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, "GET", h.IndexURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("index http %d", res.StatusCode)
	}
	var idx Index
	return &idx, json.NewDecoder(res.Body).Decode(&idx)
}

func (h *Host) download(ctx context.Context, art Artifact, dest string) error {
	hc := h.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, "GET", art.URL, nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("driver http %d", res.StatusCode)
	}
	if err := os.MkdirAll(h.driverDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(h.driverDir(), "dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hash), res.Body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if sum != art.SHA256 {
		return fmt.Errorf("驱动产物哈希不符：want %s got %s", art.SHA256[:8], sum[:8])
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ---- stdio JSON 客户端 ----

type client struct {
	h     *Host
	cmd   *exec.Cmd
	stdin io.WriteCloser
	sc    *bufio.Scanner
	scErr chan error

	wmu   sync.Mutex // 串行化请求-响应
	rmu   sync.Mutex // 保护 last
	nextID int
	last   time.Time
	dead   bool

	cols, idxs, ddls bool
}

type wireResp struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func (c *client) call(ctx context.Context, method string, params any, out any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.rmu.Lock()
	c.last = time.Now()
	c.dead = c.dead || false
	c.rmu.Unlock()

	c.nextID++
	line, err := json.Marshal(map[string]any{"id": c.nextID, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("driver %s: %w", method, err)
	}
	if !c.sc.Scan() {
		err := <-c.scErr
		if err == nil {
			err = io.EOF
		}
		return fmt.Errorf("driver %s: %v", method, err)
	}
	var res wireResp
	if err := json.Unmarshal(c.sc.Bytes(), &res); err != nil {
		return fmt.Errorf("driver %s: bad response: %w", method, err)
	}
	if res.Error != "" {
		return fmt.Errorf("driver %s: %s", method, res.Error)
	}
	if out != nil {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

type openResult struct {
	Protocol int  `json:"protocol"`
	Columns  bool `json:"columns"`
	Indexes  bool `json:"indexes"`
	DDL      bool `json:"ddl"`
}

func (c *client) open(cfg source.Config) (openResult, error) {
	var r openResult
	if err := c.call(context.Background(), "open", cfg, &r); err != nil {
		return r, err
	}
	if r.Protocol != ProtocolVersion {
		return r, fmt.Errorf("驱动协议版本 %d 与主程序 %d 不符", r.Protocol, ProtocolVersion)
	}
	c.cols, c.idxs, c.ddls = r.Columns, r.Indexes, r.DDL
	return r, nil
}

func (c *client) kill() {
	c.rmu.Lock()
	if c.dead {
		c.rmu.Unlock()
		return
	}
	c.dead = true
	c.rmu.Unlock()
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait() // 置 ProcessState，exited() 由此判断
}

func (c *client) exited() bool {
	return c.cmd.ProcessState != nil
}

func (c *client) lastUsed() time.Time {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	return c.last
}

// ---- source.Source 实现（能力标记在调用点判断） ----

// OpenFunc 返回可塞进 conn.NewManager 的构造函数：构造即完成拉起与握手，
// 随后 Manager 调用的 src.Open 是幂等 no-op。
func (h *Host) OpenFunc() func(source.Config) (source.Source, error) {
	return func(cfg source.Config) (source.Source, error) {
		return h.Open(context.Background(), cfg.Driver, cfg)
	}
}

// Open 幂等 no-op：握手已在 Host.Open 里完成（conn.Manager 构造后会再调）。
func (c *client) Open(_ context.Context, _ source.Config) error { return nil }

func (c *client) Ping(ctx context.Context) error {
	return c.call(ctx, "ping", nil, nil)
}

func (c *client) Children(ctx context.Context, path source.Path) ([]source.Node, error) {
	var r struct {
		Nodes []source.Node `json:"nodes"`
	}
	err := c.call(ctx, "children", map[string]any{"path": path}, &r)
	return r.Nodes, err
}

func (c *client) Exec(ctx context.Context, sql string, opts source.ExecOptions) (source.Result, error) {
	var r source.Result
	err := c.call(ctx, "exec", map[string]any{"sql": sql, "max_rows": opts.MaxRows}, &r)
	return r, err
}

func (c *client) Close() error {
	c.call(context.Background(), "close", nil, nil)
	c.kill()
	c.h.forget(c)
	return nil
}

func (c *client) Columns(ctx context.Context, path source.Path) ([]source.Column, error) {
	if !c.cols {
		return nil, fmt.Errorf("该数据源不支持列字段")
	}
	var r struct {
		Columns []source.Column `json:"columns"`
	}
	err := c.call(ctx, "columns", map[string]any{"path": path}, &r)
	return r.Columns, err
}

func (c *client) Indexes(ctx context.Context, path source.Path) ([]source.Index, error) {
	if !c.idxs {
		return nil, fmt.Errorf("该数据源不支持索引")
	}
	var r struct {
		Indexes []source.Index `json:"indexes"`
	}
	err := c.call(ctx, "indexes", map[string]any{"path": path}, &r)
	return r.Indexes, err
}

func (c *client) DDL(ctx context.Context, path source.Path) (string, error) {
	if !c.ddls {
		return "", fmt.Errorf("该数据源不支持 DDL")
	}
	var r struct {
		DDL string `json:"ddl"`
	}
	err := c.call(ctx, "ddl", map[string]any{"path": path}, &r)
	return r.DDL, err
}
