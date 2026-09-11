# 性能实测记录（#25，ADR-0006 两条硬线 + 万表线）

测量日期：2026-09-12。机器：macOS (Darwin 25.6, Apple Silicon)。Go 1.27、Flutter 3.47.2 release 构建。

## 三门槛

| 线 | 门槛 | 实测 | 判定 |
|---|---|---|---|
| 全进程空闲 RSS 总和 | ≤150 MB | **129.8 MB** 稳态（Flutter 界面 117.9 + sidecar 11.9；无活动驱动代理） | 达标（贴线，见下） |
| 冷启动 | ≤1s 双击到可交互 | 进程出现 **94ms**；sidecar health ok 52–71ms（5 次中位 54ms）；`推测:` 窗口首帧渲染在进程起来后 ~300ms 内，总远低于 1s | 达标 |
| 万表列表 | ≤500ms | 冷（拉全库结构+写缓存）**70ms**；热（缓存命中）9ms | 达标 |

稳态细目：启动后 6s 时总和 143.9MB，10s 后回落到 129.8MB。驱动代理非常驻（ADR-0002 空闲收割），空闲时只有两个进程。贴线对策沿用 ADR-0006：不加重 UI 库；若后续超线，退路是换 Wails。

## 复测方法

```sh
# 冷启动（sidecar）
for i in 1 2 3 4 5; do rm -rf $H/.lazydb; \
  (time lazydb -home $H & until curl health; do sleep 0.005; done; kill %1) 2>&1 | grep real; done

# 万表列表：建万表 sqlite 库，POST 连接后
time curl http://127.0.0.1:$PORT/api/connections/$ID/children?path=main   # 跑两次，第二次为缓存命中

# 全进程 RSS
ps aux | grep -E 'lazydb' | grep -v grep | awk '{s+=$6} END {print s/1024 " MB"}'
```

Flutter 界面 = `flutter build macos --release` 后 `open lazydb.app`，LAZYDB_BIN 指向编好的 sidecar。

## 注

- 真机 keychain（凭据落钥匙串）首次写入需 GUI 授权一次，无头环境无法自动验证；落盘无明文由 `TestCredentialsNotOnDisk`（全目录扫描）覆盖，逻辑同构。
