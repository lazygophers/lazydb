# ADR-0001：Flutter 界面 + Go 后端 sidecar

- 状态：已接受（2026-09-11，用户多轮确认）
- 关联卡片：#6 选语言、#7 选桌面界面框架

## 背景

约束：三平台桌面（macOS/Windows/Linux）；7 类数据源 + Kafka/MQTT + 审计平台；SQL 高亮 + 表名字段名补全（#13）；MCP 官方 SDK；内存红线见 ADR-0006。用户明确接受「前端一种语言、后端一种语言」。

关键实测（2026-09-11）：

| 量什么 | 数据 | 出处 |
|---|---|---|
| 7 类 Go 驱动全静态链接 | 11.9 MB | 本机 Go 1.27.1 实测 |
| Go `CGO_ENABLED=0` 三平台交叉编译 | 全通过 | 本机实测 |
| Wails（GoNavi）界面进程 RSS | 33.6 MB | 本机 `ps aux` |
| Flutter desktop 空闲内存 | 63–100 MB | [Reddit 实测 63MB](https://www.reddit.com/r/FlutterDev/comments/qynihb/flutter_desktop_performance/)，社区口径 60–100+ |
| Flutter SQL 编辑器 | 有高亮（[flutter_code_editor](https://akvelon.com/flutter-code-editor/)，SQL 在列）、[re_editor](https://github.com/reghuo/re_editor) 高性能内核，**补全需自行接线**，无 Monaco 等价物 | 官方/仓库页 |
| Rust 后端的 SQLite | rusqlite 必须 C 编译器（bundled 编 sqlite3.c） | `.scratch/research-drivers.md` §2（调研落盘） |
| Rust Elasticsearch 官方 crate | README 至今标 alpha | elastic/elasticsearch-rs 仓库 |

## 决策

1. **后端语言：Go**。驱动全齐且纯 Go（SQLite 用 modernc.org/sqlite）、一台机器一条命令编三平台、MCP 官方 SDK Tier 1 stable（v1.7.0）。Rust+Flutter 技术上可行（[flutter_rust_bridge](https://github.com/fzyzcjy/flutter_rust_bridge) V2 成熟）但 7 类数据源上无一胜出，弃。
2. **界面：Flutter（Dart）**，用户选定。后端不嵌进界面进程（不用 c-shared/FFI，那要开 CGO，丢掉纯 Go 交叉编译），而是 **sidecar 独立进程**，本机 HTTP + JSON 通信（127.0.0.1 随机端口，启动时由界面传入）。大数据集结果分页传输。
3. **一个 Go 二进制，两种模式**：桌面运行时作 sidecar 后端；`lazydb mcp-server` 供 AI 独立调用（ADR-0005）。

### 分层与模块边界

```
/
├── ui/                      # Flutter（Dart）：连接树、SQL 编辑器、结果表格、设置
├── cmd/lazydb/              # Go 入口：默认 sidecar 模式；子命令 mcp-server
├── internal/
│   ├── source/              # Source 接口 + 能力接口（ADR-0003）；不含任何具体驱动
│   ├── cache/               # schema 缓存：SQLite 读写、过期判断、部分刷新（ADR-0004）
│   ├── conn/                # 连接配置与凭据管理
│   ├── search/              # 结构搜索 + 查询历史搜索
│   └── driverhost/          # 驱动代理进程生命周期：拉起、健康检查、回收（ADR-0002）
└── drivers/<name>/          # 每种数据源一个驱动代理程序（独立 main）
```

边界规则：`ui/` 不 import 任何 Go 概念（只认 HTTP API）；`internal/source/` 只定义接口；具体数据源知识只存在于 `drivers/` 和审计平台适配器；`cache/` 不依赖具体驱动。

### 落选路径（什么时候该换）

- 界面内存失控、或 SQL 补全自研成本超预期 → 换 Wails v2（Go+TS，GoNavi 已验证，Monaco 白送）。
- 需要移动端 → 本架构天然支持（Flutter 本身跨端，后端换远程部署）。

## 后果

- 两种语言（Dart + Go）、Flutter 构建链入项目。
- SQL 补全要在 Flutter 侧基于 re_editor 的 API 自行实现（数据来自后端 schema 缓存，接线量可控）。
- 内存红线按全进程总和计，风险与对策见 ADR-0006。
- 冷启动为双进程：界面与后端并行拉起，`推测:` 可守 ≤1s，tracer bullet 阶段实测。

### 第一条链路（tracer bullet）

MySQL：连接 → 缓存落盘 → 界面树展示 → 执行 SQL 出结果 → `lazydb mcp-server` 查到同一份缓存。跑通即验证分层、缓存并发、MCP 三件事。
