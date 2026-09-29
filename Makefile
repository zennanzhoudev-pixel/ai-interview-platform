.PHONY: help run serve restart restart-clean selftest test test-race test-mysql vet lint fmt tidy build docker-up docker-down clean loadtest frontend-check

GO ?= go
BIN := bin/interviewd

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

run: ## 本地跑一场模拟文本面试, 输出面试报告 JSON
	$(GO) run ./cmd/interviewd -round 1 -minutes 45 -out ./bin/report.json

serve: ## 启动 Web 服务(浏览器打开 http://localhost:8080)
	$(GO) run ./cmd/interviewd -serve :8080

restart: ## 把本机 8101 上的旧进程换成当前代码构建的新版(改完前端必须做这一步)
	./scripts/restart-local.sh 8101

restart-clean: ## 同上, 并清掉 8111/8112/8113 的验证残留进程
	./scripts/restart-local.sh 8101 --clean-legacy

selftest: ## 联调自检: 探测已配置的 LLM / ASR / TTS / Embedding 是否真的可用
	$(GO) run ./cmd/interviewd -selftest

test: ## 跑单元测试
	$(GO) test ./... -count=1

test-race: ## 跑单元测试(带竞态检测)
	$(GO) test ./... -race -count=1

test-mysql: ## 跑 MySQL 集成测试(需要先 docker compose up mysql)
	MYSQL_DSN='root:root@tcp(127.0.0.1:3306)/interview?parseTime=true&loc=UTC' \
		$(GO) test ./internal/store/... -run MySQL -v -count=1

vet: ## 静态检查
	$(GO) vet ./...

lint: ## 更严格的静态检查(需要 golangci-lint)
	golangci-lint run ./...

frontend-check: ## 前端模块语法检查(没有构建步骤, 因此需要显式校验)
	@mkdir -p /tmp/jscheck-ai-interview
	@for f in web/js/*.js; do cp "$$f" "/tmp/jscheck-ai-interview/$$(basename $$f .js).mjs"; done
	@for f in /tmp/jscheck-ai-interview/*.mjs; do node --check "$$f"; done
	@echo "前端模块语法检查通过"

loadtest: ## 并发压测: 同时开 10 场完整面试
	node deployments/loadtest.mjs --base http://localhost:8080 --sessions 10

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
