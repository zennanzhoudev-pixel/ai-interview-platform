# 贡献指南

感谢参与。提交前请确认下面几件事。

## 开发流程

1. Fork 仓库并从 `main` 切分支, 分支名建议 `codex/<主题>`。
2. 本地跑通门禁:

```bash
make test
make vet
make frontend-check
```

3. 提交信息使用 Conventional Commits:

```
feat(scope): 说明
fix(scope): 说明
docs: 说明
test(scope): 说明
```

4. 发起 PR, 描述里写清: 改了什么、为什么、如何验证。

## PR 检查清单

- [ ] `make test` 通过
- [ ] `make vet` 通过
- [ ] `make frontend-check` 通过 (改了前端时)
- [ ] 新增/修改的配置已写入文档
- [ ] 降级行为可见, 没有静默失败
- [ ] 没有把密钥、真实手机号、录音等敏感数据提交进仓库
- [ ] 改了 `web/` 时, 已 `make build` 并重启验证

## 代码约定

- 所有地址/密钥/模型名/阈值走环境变量。
- 共享状态必须并发安全; 后台 goroutine 用 `safeGo` 兜底 panic。
- 新增厂商实现时实现既有接口, 不要在核心链路里写 `if 厂商 == ...`。
- 文档与行为保持一致: 行为变了, 文档和测试一起改。

## 安全

发现安全问题请不要开公开 Issue, 按 [SECURITY.md](SECURITY.md) 的方式私下报告。
