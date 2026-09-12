# 写一个新驱动代理（ADR-0002）

一种数据源 = 一个独立 Go 二进制，主程序（lazydb）经 stdio 行分隔 JSON 调用。协议与 MCP 子命令同构：一个请求一行 JSON、一个响应一行 JSON，`id` 配对。

## 进程协议

首个请求必须是 `open`（带完整连接配置），此后同一进程服务该连接直到 stdin 关闭：

```
→ {"id":1,"method":"open","params":{"driver":"mysql","dsn":"...","ssh":{...}}}
← {"id":1,"result":{"protocol":2,"columns":true,"indexes":true,"ddl":true,"readonly":true}}
```

| method | params | 说明 |
|---|---|---|
| `open` | `source.Config`（driver/dsn/ssh） | 建连接；result 含协议版本与四个能力位 |
| `ping` | — | 探活 |
| `children` | `{"path":["main"]}` | 结构树逐层：空 path = 顶层（库），到表为止 |
| `exec` | `{"sql":"...","max_rows":100}` | 执行；result = `source.Result`（columns/rows/truncated/rows_affected） |
| `exec_readonly` | `{"sql":"...","max_rows":100}` | 事务内执行并永远回滚（#30）；仅 `open` readonly 位为 true 时可用 |
| `columns` / `indexes` / `ddl` | `{"path":["db","table"]}` | 仅 `open` 能力位为 true 时可用 |
| `close` | — | 关连接；之后进程可退出 |

能力位：不支持就回 false，主程序调用点会报「该数据源不支持 X」，不会崩。

## 怎么写

1. 实现接口（`internal/source`）：
   - `Source`：Open/Close/Ping/Children/Exec（必需）
   - `ColumnLister`/`IndexLister`/`DDLShower`/`ReadOnlyExecer`（可选，能力接口按需实现）
   - `RowStreamer`（可选，导出流式用）
2. 服务端不用自己写循环：`driveragent.Run(ctx, os.Stdin, os.Stdout)`，它对 `open` 传进来的 Config 做 `builtin.Open` 式分发——把你的构造函数注册进 `drivers/builtin/builtin.go` 的 switch，或自己 fork 一份 `cmd/lazydb-driver` 做入口。
3. 单测照抄 `internal/driverhost/host_test.go` 的 re-exec 模式：`TestMain` 里检查环境变量，子进程跑 `driveragent.Run`，父进程经 driverhost 全链路（下载/spawn/exec/回收）打真协议。

## 发布（index）

主程序从索引下载驱动二进制（`LAZYDB_DRIVER_INDEX` 指向 index JSON URL）：

```json
{
  "drivers": {
    "clickhouse": {
      "version": "1.0.0",
      "protocol": [1, 2],
      "platforms": {
        "darwin-arm64": {"url": "https://…/lazydb-driver-clickhouse-darwin-arm64", "sha256": "…"}
      }
    }
  }
}
```

协议区间与主程序（`driverhost` 当前 1）无交集的驱动会被拒；本地哈希不符拒；有可用的本地二进制时离线可直接用。产物命名 `lazydb-driver-<name>-<os>-<arch>`。

## 参考

- `internal/driveragent/agent.go`：协议服务端
- `internal/driverhost/host.go`：主程序侧（下载/生命周期）
- `internal/mysqlsrc/mysql.go`：内置 MySQL 驱动（含 SSH 隧道写法）
