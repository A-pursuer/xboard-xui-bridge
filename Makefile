# xboard-xui-bridge 构建脚本。
#
# 设计原则：
#   - 单一 Make 目标对应单一动作；不在一个目标里做两件事。
#   - 所有 Go 命令都通过环境变量 GO 引用，便于切换 toolchain。
#   - 版本号通过 ldflags 注入到 main.version；CI 构建时由 git tag 提供。
#
# v0.2 起的目标增量：
#   - web         本地构建 Vue 3 前端 + 拷贝到 internal/web/dist 供 go:embed
#   - build-all   先 web 再 build，端到端产出含前端的二进制
#
# 平台说明：
#   本 Makefile 假设 GNU Make + Unix 工具（cp / rm / mkdir）。Windows 用户
#   建议用 WSL 或 git-bash 运行；纯 cmd 环境请直接用 Docker 多阶段构建。

GO        ?= go
NPM       ?= npm
GOFLAGS   ?= -trimpath
LDFLAGS   ?= -s -w -X main.version=$(VERSION)
VERSION   ?= $(shell git -C $(CURDIR) describe --tags --always 2>/dev/null || echo dev)
OUT       ?= dist
BIN       ?= xboard-xui-bridge

WEB_DIR   := web
EMBED_DIR := internal/web/dist

.PHONY: all
all: build-all

## build: 编译当前平台二进制到 dist/<bin>（不含前端构建步骤；用 build-all 端到端）
.PHONY: build
build:
	@mkdir -p $(OUT)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN) ./cmd/bridge

## build-linux: 交叉编译 linux/amd64 二进制
.PHONY: build-linux
build-linux:
	@mkdir -p $(OUT)
	GOOS=linux GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-linux-amd64 ./cmd/bridge

## build-arm64: 交叉编译 linux/arm64 二进制（适用于 ARM 架构服务器）
.PHONY: build-arm64
build-arm64:
	@mkdir -p $(OUT)
	GOOS=linux GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-linux-arm64 ./cmd/bridge

## build-arm: 交叉编译 linux/armv7 二进制（树莓派 32-bit / 老 ARM SBC）
.PHONY: build-arm
build-arm:
	@mkdir -p $(OUT)
	GOOS=linux GOARCH=arm GOARM=7 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-linux-arm ./cmd/bridge

## build-darwin: 交叉编译 macOS 双架构二进制（开发测试用，不推荐生产部署）
.PHONY: build-darwin
build-darwin:
	@mkdir -p $(OUT)
	GOOS=darwin GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-darwin-amd64 ./cmd/bridge
	GOOS=darwin GOARCH=arm64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-darwin-arm64 ./cmd/bridge

## build-windows: 交叉编译 Windows amd64 二进制（开发测试用）
.PHONY: build-windows
build-windows:
	@mkdir -p $(OUT)
	GOOS=windows GOARCH=amd64 $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(OUT)/$(BIN)-windows-amd64.exe ./cmd/bridge

## build-release: 构建全部发布平台（linux 三架构 + darwin 双架构 + windows amd64）
##
## 用法：make web && make build-release VERSION=v0.8.4
## 输出：dist/$(BIN)-{linux-amd64,linux-arm64,linux-arm,darwin-amd64,darwin-arm64,windows-amd64.exe}
.PHONY: build-release
build-release: build-linux build-arm64 build-arm build-darwin build-windows
	@echo "release binaries:"
	@ls -1 $(OUT) | sed 's/^/  /'

## release-tars: 把 build-release 产出的裸二进制打包为 install.sh 期望的 tar.gz
##
## install.sh 下载 https://.../xboard-xui-bridge-${arch}.tar.gz 并 tar -xzf 后期望
## 内部有一个名为 `xboard-xui-bridge` 的二进制；本 target 把每个平台的裸二进制
## 临时改名后 tar，并生成 SHA256SUMS.txt 供安装脚本校验。
##
## 输出：dist-release/xboard-xui-bridge-${arch}.tar.gz + SHA256SUMS.txt
##
## 用法：make web && make build-release release-tars VERSION=v0.8.4
.PHONY: release-tars
release-tars:
	@mkdir -p dist-release
	@rm -f dist-release/*.tar.gz dist-release/SHA256SUMS.txt
	@set -e; \
	for asset in linux-amd64 linux-arm64 linux-arm darwin-amd64 darwin-arm64; do \
		src=$(OUT)/$(BIN)-$$asset; \
		if [ ! -f "$$src" ]; then \
			echo "skip $$asset: $$src not found"; continue; \
		fi; \
		tmp=$$(mktemp -d); \
		cp "$$src" "$$tmp/$(BIN)"; \
		chmod 0755 "$$tmp/$(BIN)"; \
		tar -czf "dist-release/$(BIN)-$$asset.tar.gz" -C "$$tmp" "$(BIN)"; \
		rm -rf "$$tmp"; \
		echo "packed $$asset"; \
	done; \
	if [ -f $(OUT)/$(BIN)-windows-amd64.exe ]; then \
		tmp=$$(mktemp -d); \
		cp $(OUT)/$(BIN)-windows-amd64.exe "$$tmp/$(BIN).exe"; \
		tar -czf "dist-release/$(BIN)-windows-amd64.tar.gz" -C "$$tmp" "$(BIN).exe"; \
		rm -rf "$$tmp"; \
		echo "packed windows-amd64"; \
	fi
	@cd dist-release && for f in *.tar.gz; do \
		hash=$$(sha256sum "$$f" | awk '{print $$1}'); \
		printf "%s  %s\n" "$$hash" "$$f"; \
	done > SHA256SUMS.txt
	@echo "release artifacts:"
	@ls -1 dist-release | sed 's/^/  /'

## release: 端到端发布构建（web + 全平台二进制 + tar.gz + SHA256SUMS.txt）
##
## 用法：make release VERSION=v0.8.4
##
## 用 sub-make 串行执行而非 prerequisites 并列——避免 `make -j release`
## 并行模式下 release-tars 早于 build-release 执行导致打包失败。
.PHONY: release
release:
	$(MAKE) web
	$(MAKE) build-release
	$(MAKE) release-tars

## web: 构建 Vue 3 前端并把 dist 同步到 internal/web/dist 让 go:embed 打入二进制
##
## 先 npm ci（严格按 package-lock.json 安装，CI / 本地依赖树一致、构建可
## 复现）→ npm run build 输出到 web/dist → rm -rf internal/web/dist + cp
## 全量覆盖。注意：本目标不增量——每次都重建并全量覆盖，保证 dist 永远
## 反映最新前端代码。
.PHONY: web
web:
	cd $(WEB_DIR) && $(NPM) ci
	cd $(WEB_DIR) && $(NPM) run build
	rm -rf $(EMBED_DIR)
	mkdir -p $(EMBED_DIR)
	cp -R $(WEB_DIR)/dist/. $(EMBED_DIR)/

## build-all: 先构建前端，再编译 Go 二进制；端到端产出含 GUI 的二进制
##
## 这是开发期"想看完整效果"的标准目标。CI / Docker 构建路径不走本目标，
## 它们各自有更精确的多阶段流程。
.PHONY: build-all
build-all: web build

## test: 跑全部 Go 单元测试
.PHONY: test
test:
	$(GO) test ./...

## vet: 静态检查
.PHONY: vet
vet:
	$(GO) vet ./...

## tidy: 清理 go.mod / go.sum
.PHONY: tidy
tidy:
	$(GO) mod tidy

## clean: 清理 Go 产物 + 前端构建中间产物（不动 internal/web/dist——它是 commit 入仓的副本）
.PHONY: clean
clean:
	rm -rf $(OUT)
	rm -rf $(WEB_DIR)/dist
	rm -rf $(WEB_DIR)/node_modules

## help: 列出常用目标
.PHONY: help
help:
	@grep -E '^##' Makefile | sed -e 's/## /  /'
