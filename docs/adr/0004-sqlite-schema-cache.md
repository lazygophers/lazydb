# ADR-0004：schema 缓存——单个 SQLite 文件

- 状态：已接受（2026-09-11）
- 关联卡片：#11 架构文档（缓存形态在 wf-final 定稿）

## 背景

需求：所有连接的库/表/字段/索引/外键 + 时间戳；按表过期、部分刷新；单连接可达 10000 表 × 20 字段 ≈ 20 万行；命中时表列表 ≤500ms；模糊搜索要全量扫名字。对照：DBeaver 不落盘（内存 List+Map，`dbeaver/dbeaver` 的 `AbstractObjectCache`）——本地缓存是 lazydb 的差异点之一。SQLite 纯 Go 驱动（modernc.org/sqlite）本来就在依赖内，零额外成本。

## 决策

- 全部连接的元数据进**一个** `~/.lazydb/cache.db`，SQLite，**WAL 模式 + 文件锁**，界面与 `mcp-server` 并发读写。
- 表结构（示意）：`objects(conn, kind, path, fetched_at)` + `columns(...) / indexes(...)`；`fetched_at` 支撑「N 天过期、单表强制刷新」。
- 模糊搜索：启动时把当前连接的结构名字 load 进内存索引（20 万行 `推测:` 数十 MB 以内），搜索走内存；落盘只服务持久化与并发。

### 落选形态

- **每连接一个 JSON**：改一张表重写整个文件。**二进制文件**：不可读、自造格式。都只在「SQLite 驱动不在依赖里」的平行世界才对。

## 后果

- 缓存损坏的兜底 = 删文件重建（结构可重拉，无损）。
- modernc 写入性能约为原生 SQLite 一半（2022 DataStation 基准，现行版本待实测）；本场景是元数据写入，吞吐无关紧要。
