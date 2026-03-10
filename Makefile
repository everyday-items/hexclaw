# HexClaw Makefile
#
# 使用: make help

# 版本信息
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)"

# 二进制输出目录
BIN_DIR := bin

.PHONY: all build run test lint fmt vet clean help init

## 默认目标：构建
all: build

## 构建 hexclaw 二进制
build:
	@mkdir -p $(BIN_DIR)
	go build $(LDFLAGS) -o $(BIN_DIR)/hexclaw ./cmd/hexclaw/

## 运行服务（开发模式）
run: build
	$(BIN_DIR)/hexclaw serve

## 运行所有测试
test:
	go test ./...

## 运行测试并输出覆盖率
test-cover:
	go test -cover ./...

## 代码格式化
fmt:
	go fmt ./...

## 代码静态检查
vet:
	go vet ./...

## golangci-lint 检查
lint:
	golangci-lint run

## 清理构建产物
clean:
	rm -rf $(BIN_DIR)

## 初始化配置
init: build
	$(BIN_DIR)/hexclaw init

## 显示帮助
help:
	@echo "HexClaw - 企业级安全的个人 AI Agent"
	@echo ""
	@echo "可用目标:"
	@echo "  make build       构建二进制"
	@echo "  make run         构建并运行服务"
	@echo "  make test        运行测试"
	@echo "  make test-cover  运行测试（含覆盖率）"
	@echo "  make fmt         格式化代码"
	@echo "  make vet         静态检查"
	@echo "  make lint        golangci-lint 检查"
	@echo "  make clean       清理构建产物"
	@echo "  make init        初始化配置"
