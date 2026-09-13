# lazydb：一个 Go 二进制（sidecar / mcp-server）+ Flutter 界面（ui/）。
# CI 等价物见 .github/workflows/ci.yml。

UNAME := $(shell uname -s | tr '[:upper:]' '[:lower:]')
DESKTOP := $(if $(filter darwin,$(UNAME)),macos,linux)
# macOS Keychain 后端要求 cgo（99designs/keyring keychain.go 带 darwin&&cgo
# 构建标签），关掉 cgo 编出来的二进制不持久化连接凭据；其余平台保持静态。
CGO := $(if $(filter darwin,$(UNAME)),1,0)

# make run 开发模式常量：固定地址 + 固定令牌，后端重启后界面无需重附着
DEV_ADDR := 127.0.0.1:52187
DEV_TOKEN := lazydb-dev-token

.PHONY: help lint test run build build-go

help: ## 列出所有目标
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

lint: ## go vet + flutter analyze
	go vet ./...
	cd ui && flutter analyze

test: ## go 全量测试 + flutter widget 测试
	go test -count=1 ./...
	cd ui && flutter test

# 开发模式：后端先起并探活，界面再附着（不会自己再拉一个）。
# Go 改动 → 重编 → TERM 旧后端（走优雅退出回收驱动代理）→ 起；
# Dart 改动 → 向 flutter run 的 stdin 发 R 触发热重启。
# 注意：后端重启会丢连接（连接握在旧进程里），界面里重连即可。
run: ## 开发模式：后端 + 界面一起跑，双侧监听热重启
	@CGO_ENABLED=$(CGO) go build -o lazydb-dev ./cmd/lazydb
	@trap 'kill 0' INT TERM EXIT; \
	backend_start() { \
		LAZYDB_TOKEN=$(DEV_TOKEN) ./lazydb-dev -addr $(DEV_ADDR) & echo $$! > .backend.pid; \
		for i in $$(seq 1 50); do \
			curl -sf -H 'Authorization: Bearer $(DEV_TOKEN)' http://$(DEV_ADDR)/api/health >/dev/null && return 0; \
			sleep 0.2; \
		done; \
		echo '[watch] 后端 10s 内未就绪' >&2; return 1; \
	}; \
	backend_kill() { \
		[ -f .backend.pid ] || return 0; \
		pid=$$(cat .backend.pid); \
		if kill -0 $$pid 2>/dev/null; then \
			kill -TERM $$pid; \
			for i in $$(seq 1 15); do kill -0 $$pid 2>/dev/null || break; sleep 0.2; done; \
			kill -9 $$pid 2>/dev/null; \
			wait $$pid 2>/dev/null; \
		fi; \
		rm -f .backend.pid; \
	}; \
	pkill -TERM -f 'lazydb-dev -addr $(DEV_ADDR)' 2>/dev/null && sleep 1; \
	touch .watch-go .watch-dart; \
	backend_start || exit 1; \
	( while sleep 1; do \
		find cmd internal drivers -name '*.go' -newer .watch-go 2>/dev/null | grep -q . || continue; \
		touch .watch-go; \
		echo '[watch] Go 变化：重编并重启后端'; \
		CGO_ENABLED=$(CGO) go build -o lazydb-dev ./cmd/lazydb || { echo '[watch] 编译失败，保留旧后端'; continue; }; \
		backend_kill; backend_start; \
	done ) & \
	( while sleep 1; do \
		find ui/lib -name '*.dart' -newer .watch-dart 2>/dev/null | grep -q . || continue; \
		touch .watch-dart; echo R; \
	done ) | ( cd ui && LAZYDB_BIN=$(CURDIR)/lazydb-dev flutter run -d $(DESKTOP) )

build: build-go ## 构建 Go 二进制 + flutter 桌面产物
	cd ui && flutter build $(DESKTOP)

build-go: ## 只构建 Go 二进制（lazydb + lazydb-driver）
	CGO_ENABLED=$(CGO) go build -o lazydb ./cmd/lazydb
	CGO_ENABLED=$(CGO) go build -o lazydb-driver ./cmd/lazydb-driver
