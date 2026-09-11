# lazydb 桌面界面

Flutter 桌面应用（macOS / Windows / Linux）。界面零业务逻辑：附着或拉起 Go sidecar
（`cmd/lazydb`），一切数据经本机 HTTP API。

## 运行

```sh
# 1. 先编 sidecar（app 启动时找不到后端会自己拉起）
CGO_ENABLED=0 go build -o lazydb ./cmd/lazydb

# 2. 跑界面（在仓库根目录，app 会用 ./lazydb）
export LAZYDB_BIN=$PWD/lazydb   # 或直接把 lazydb 放进 PATH
cd ui && flutter run -d macos   # windows / linux 同理
```

发现顺序：`LAZYDB_BIN` → 当前目录 `lazydb` → 可执行文件同目录 → `PATH`。
