# ADR-0003：数据源抽象——核心接口 + 能力接口

- 状态：已接受（2026-09-11）
- 关联卡片：#9 抽象层形状、#4 审计平台接口、#5 搜索语义

## 背景

要统一：关系型、分析型、键值、文档、搜索、消息队列、审计平台（HTTP）。#5 已定不做表内数据搜索，**抽象层无需表达查询条件、无需条件下推**，只剩两件事：列结构、执行语句。参考 Bytebase 的 10 方法 `Driver` 接口（`bytebase/bytebase:backend/plugin/db/driver.go`）与 Archery 同步 REST 的实测（`/api/v1/sqlquery/resources/` 元数据齐全）。

## 决策

核心接口（每类数据源必须实现）：

```go
type Source interface {
    Open(ctx context.Context, cfg Config) error
    Close() error
    Ping(ctx context.Context) error
    Children(ctx context.Context, path Path) ([]Node, error) // 列结构：实例→库→表
    Exec(ctx context.Context, stmt string, opts ExecOptions) (Result, error)
}
```

能力接口（谁能谁实现，上层类型断言探测）：

```go
type ColumnLister interface{ Columns(ctx, table Path) ([]Column, error) }
type IndexLister  interface{ Indexes(ctx, table Path) ([]Index, error) }
type DDLShower    interface{ DDL(ctx, obj Path) (string, error) }
```

- Redis 实现 `Children` 返回 db 编号、Kafka 返回 topic 列表——它们对能力接口全部不支持，不是空实现。
- 审计平台实现同一 `Source`（Archery 同步返回，天然吻合），写操作返回「不支持」。

### 落选形状

- **单一大接口 + 「不支持」返回值**（Bytebase 形）：迫使 Redis 写一堆无意义方法。换的条件：若能力接口数量涨到 8+ 且上层断言遍地开花，合并回单接口。
- **按类别多接口**：上层为每类写一套代码，共用逻辑消失。

## 后果

- 上层代码需要 `switch ok := s.(ColumnLister)` 式探测，集中在树 UI 一处。
- 流数据源（Kafka/MQTT）的 `Exec` 语义 = 「订阅/查询窗口」，在接第二个非关系源时细化。
