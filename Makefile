.PHONY: help run test vet fmt tidy build docker-up docker-down clean

GO ?= go
BIN := bin/interviewd

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

run: ## 本地跑一场模拟文本面试, 输出面试报告 JSON
	$(GO) run ./cmd/interviewd -round 1 -minutes 45 -out ./bin/report.json

test: ## 跑单元测试
	$(GO) test ./... -race -count=1

vet: ## 静态检查
	$(GO) vet ./...

fmt: ## 格式化
	$(GO) fmt ./...

tidy: ## 整理依赖
	$(GO) mod tidy

build: ## 编译二进制
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/interviewd

docker-up: ## 起本地依赖(mysql/redis/es/kafka/jaeger/prometheus/grafana)
	docker compose up -d

docker-down: ## 停本地依赖
	docker compose down

clean: ## 清理构建产物
	rm -rf bin
