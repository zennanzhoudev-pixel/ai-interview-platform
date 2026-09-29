# AI Interview OS · 企业级 AI 线上面试中台

把组织里的 **1 面到 5 面**标准化为可编排的面试流程: AI 承担提问、追问与评分,
人类面试官在自己该出现的那一轮进入面试间; 面试结论自动回流到招聘管道,
所有人对同一份数据做判断。

`Go 1.22` · `WebSocket(文本+音频+视频信令)` · `WebRTC` · `MySQL` · `Redis` ·
`Prometheus` · `OpenTelemetry` · 零构建前端(go:embed)

---

## 1. 它解决什么问题

技术面试的前一两轮里, 大量时间花在**重复、可标准化**的环节: 简历真实性核对、
基础技术深挖、编码。而真正需要人的判断力的环节(架构取舍、协作方式、动机匹配)
常常因为前面的环节挤占时间而被草草带过。

所以核心主张是:

> **AI 不是替代面试官, 而是重构面试流水线** —— 1 到 2 面尽量自动化,
> 3 到 5 面变成人机协同, 人类面试官的注意力只花在真正需要判断的地方。

| 轮次 | 考察目标 | AI 角色 | AI 参与度 | 人类角色 |
|---|---|---|---|---|
| 1 面 | 简历真实性、语言/框架基础 | 全自动 AI 面试官 | 100% | 仅异常复核 |
| 2 面 | 现场编码、边界与复杂度 | AI 监考 + 判题(隔离沙箱) | 90% | 代码抽检 |
| 3 面 | 架构设计、容量估算 | AI 主持 + 抬杠式追问 + 旁听席 | 60% | 人类终审 |
| 4 面 | 业务理解、选型决策 | 纪要 + 证据整理 | 30% | 人类主导 |
| 5 面 | 动机、稳定性、文化匹配 | 初筛 + 纪要 | 20% | HR 主导 |

轮次怎么跑**由职位的 `rounds` 配置决定**(哪轮 AI 主导、哪轮需要真人到场、
哪轮是编程轮), 不是写死在代码里。

---

## 2. 现在能跑到什么程度

下面这张表是**如实的完成度**, 不是路线图。带 ⚠️ 的项需要你自己的密钥或环境才能真正跑通。

| 能力 | 状态 | 说明 |
|---|---|---|
| 面试编排(阶段状态机 + 问题 DAG + 预算/覆盖度) | ✅ 可运行 | 断线重连靠重放已落库问答, 可跨进程恢复 |
| RAG 参考题库检索(BM25 + 向量 + RRF + 精排) | ✅ 可运行 | 追问方向来自检索结果; 工作台有"检索可视化"入口 |
| 简历解析与原文定位 | ✅ 可运行 | 解析成带 rune 偏移的实体, 参与追问与报告定位 |
| 浏览器麦克风采集与音频上行 | ✅ 可运行 | 16kHz/20ms PCM16 帧, 前端 VAD 联动打断 |
| 服务端 VAD / 流式识别 / TTS / 打断 | ✅ 可运行 | 需要配置厂商密钥; 未配置时明确拒绝而不是静默降级 |
| 视频面试 | ✅ 可运行 | 候选人自视 + 人类面试官 P2P 视频(WebRTC, 媒体不过服务端) |
| 面试录像 | ✅ 可运行 | 浏览器分片上传 + Range 回放 + 保留期清理 + 播放留痕 |
| 编程轮次与判题沙箱 | ✅ 可运行 | ⚠️ 生产需要容器运行时; 无容器时降级为本机执行并如实告警 |
| 多租户 RBAC(4 个权限点组 × 3 角色) | ✅ 可运行 | 租户只来自凭据, 绝不接受请求参数 |
| 审计日志落库(只追加) | ✅ 可运行 | 面试创建/报告查看/改分/录像播放/密钥操作全部留痕 |
| 限流 / panic 兜底 / 结构化日志 / 指标 / 链路追踪 | ✅ 可运行 | Prometheus 25+ 组指标, OTLP 未配置时退化为 no-op |
| 招聘域: 职位 / 候选人 / 投递管道 / 题库 / 排期 | ✅ 可运行 | 面试结论自动回流到管道; AI 只记录事实, 不做自动淘汰 |
| 报告: 证据绑定 + 人工改分 + 打印/导出 | ✅ 可运行 | 每个维度分必须挂候选人原话 |
| 真实大模型评分(双模型交叉 + 仲裁) | ⚠️ 需密钥 | 配置 `LLM_API_KEY` 后启用; 未配置时用规则评分器并标注 |
| 语义向量检索 | ⚠️ 需密钥 | 配置 `EMBEDDING_API_KEY`; 未配置时用本地特征哈希(词面相似度) |
| 真实厂商 ASR/TTS 联调 | ⚠️ 需密钥 | 适配器走 OpenAI 兼容接口, 用 `-selftest` 可先自检 |

### 刻意不做的事

这些不是"还没做", 而是产品边界:

- **不做 AI 自动淘汰**: 反作弊与评分只产出建议与风险事件, 录用决定必须由人做出并署名。
- **不做 AI 伪装真人**: 面试开始时就明确告知本轮由 AI 主持。
- **不做情绪识别与面相分析**: 它们没有可靠依据, 却会被当成客观结论使用。
- **不做"录像永久保留"**: 保留期是默认值(90 天)而不是可选项。

---

## 3. 快速开始

### 3.1 零依赖跑一场完整面试(不需要任何密钥)

```bash
make run          # 离线模拟一场面试, 输出报告 JSON
make test         # 全部单元测试(含沙箱真实执行、录像分片、租户隔离契约)
```

### 3.2 起服务, 用浏览器跑完整链路

```bash
make serve        # http://localhost:8080
```

> **前端是编译进二进制的。** 这是刻意的设计(部署只有一个文件、不依赖 CDN,
> 面试页面不会因为外网挂了白屏), 代价是: 改了 `web/` 下面的任何文件,
> 都必须重新构建并重启进程, 浏览器才会看到新界面 —— 硬刷新也没用,
> 因为服务端发出去的还是旧的那份。
>
> 已经有一个本地服务在跑(比如 8101)时, 用这一条命令换掉它:
>
> ```bash
> make restart          # 停掉 8101 上的旧进程 -> 重新构建 -> 启动新版
> make restart-clean    # 同上, 并清掉 8111/8112/8113 之类的历史残留进程
> ```
>
> 想确认浏览器拿到的是新前端, 可以直接问服务端:
>
> ```bash
> curl -s localhost:8101/ | grep -c "/js/app.js"   # 输出 1 = 新版; 0 = 旧版
> ```

打开 `http://localhost:8080` 后:

1. **产品介绍** 是完整的产品说明(流程、RAG、评分、视频、边界)。
2. **招聘工作台** 在本地演示模式下会自动放行(未开启鉴权时不需要密钥)。
   新建职位 → 导入候选人 → 创建投递 → 面试安排 → **开始面试** → 复制候选人链接。
3. 用复制出来的链接打开 **候选人空间**: 设备检查 → 数据授权 → 简历确认 → **进入面试间**。
   面试间支持摄像头自视、麦克风电平、语音作答(直接说话)、键盘作答、编程面板(二面)。
4. 面试结束后自动生成报告: 分数、能力维度、逐条原话证据、简历原文定位、打印/导出。
5. 报告与管道数据会回到工作台的 **报告** 与 **管道** 页。

### 3.3 起完整依赖(MySQL / Redis / 录制存储 / Jaeger / Prometheus / Grafana)

```bash
make docker-up    # 包含 interviewd 本体
make test-mysql   # 存储契约测试(内存实现与 MySQL 实现跑同一套用例)
```

---

## 4. 配置

所有配置走环境变量或命令行参数, **没有配置文件** —— 少一个配置文件就少一处
"本地能跑线上不能跑"。

| 变量 | 作用 | 不配置时的行为 |
|---|---|---|
| `MYSQL_DSN` | 业务主数据存储 | 用内存存储(重启丢数据), 并自动开启鉴权警告 |
| `REDIS_ADDR` | 会话快照(断线重连加速) | 不启用快照; 重连仍然可用, 只是多读一次数据库 |
| `APP_SECRET` | 候选人令牌签名 + 候选人假名化密钥 | 进程内临时密钥(重启后已发出的链接失效) |
| `TENANT_ID` | 默认租户 | `default` |
| `LLM_API_KEY` / `LLM_MODEL` / `LLM_BASE_URL` | 大模型评分 | 规则评分器(报告里标注来源) |
| `LLM_MODEL_B` | 复核模型(双模型交叉) | 复核用规则评分器 |
| `EMBEDDING_API_KEY` / `EMBEDDING_MODEL` | 语义向量检索 | 本地特征哈希(词面相似度) |
| `ASR_API_KEY` / `ASR_BASE_URL` / `ASR_MODEL` | 语音识别 | 语音模式关闭(WebSocket 明确报错, 不静默降级) |
| `TTS_API_KEY` / `TTS_BASE_URL` / `TTS_MODEL` | 语音合成 | 同上 |
| `RECORDING_DIR` | 面试录像落盘目录; 设 `off` 关闭录制 | `data/recordings` |
| `RECORDING_RETENTION_DAYS` | 录像保留天数 | `90` |
| `SANDBOX_ENGINE` | 判题引擎 `auto` / `docker` / `local` | `auto`: 有容器用容器, 否则本机执行并告警 |
| `ICE_SERVERS` | WebRTC 的 STUN/TURN, 逗号分隔 | 公共 STUN(内网环境大概率需要换成自己的 TURN) |
| `OTLP_ENDPOINT` | 链路追踪接收地址 | 不导出追踪 |
| `LOG_LEVEL` / `LOG_FORMAT` | 日志级别与格式 | `info` / `text` |

开面之前先做一次上游自检, 比让第一场面试替你冒烟划算得多:

```bash
make selftest     # 逐个探测 LLM / Embedding / TTS, 并说明 ASR 为什么无法离线探测
```

---

## 5. 架构

```
候选人浏览器                     人类面试官浏览器
  视频/音频/PCM16 上行             旁听席(WebRTC 视频 + 实时转写)
  键盘作答 / 编程面板                        |
        |                                   |
        +------------- WebSocket -----------+
                        |
        接入层 internal/api  (鉴权 · 租户 · 审计 · 限流 · panic 兜底 · 指标)
                        |
   编排 internal/orchestrator (阶段状态机 + 问题 DAG + 预算/覆盖度 + 追问策略)
                        |
   AI 能力: internal/knowledge(RAG) · internal/scoring(rubric/双模型/仲裁/降级)
            internal/resume(原文定位) · internal/sandbox(隔离判题) · internal/llm
                        |
   实时媒体 internal/media (VAD · 流式 ASR · TTS · 打断级联取消)
                        |
   数据 internal/store (内存 / MySQL 同一套契约) · internal/recording(分片 · Range · 保留期)
                        |
   治理 internal/auth · internal/privacy · internal/observability · internal/platform
```

### 几个值得单独说的设计

**一条 WebSocket 承载三条链路。** 文本(JSON)、音频(二进制 PCM16)、
视频信令(WebRTC offer/answer/ICE)复用同一个连接。面试间只需要一个端口、
一次鉴权、一条重连逻辑; 多开一条通道就多一整套失败模式。

**信令走服务端, 媒体不过服务端。** 人类面试官与候选人之间是 P2P 视频,
服务端只转发 SDP/ICE。这既省掉了媒体服务器的带宽与运维成本, 也意味着
服务端看不到画面 —— 看不到的东西就不会泄漏。旁听票据由 API Key 换取,
只对一场面试、一个角色、两小时有效, 因此企业密钥不会出现在 URL 里。

**追问方向由检索决定, 不由模型发挥。** 每道题的参考答案被拆成要点建索引
(BM25 + 向量 + RRF + 精排), 只有"候选人没说到、又被参考答案强调"的要点
才会成为追问靶子。工作台里的"检索可视化"用的就是这个索引 —— 如果那里的
结果不相关, 追问也不会好。

**题目、评分要点、检索语料是同一份数据。** 如果"问的题"和"评的分"来自两处,
报告里的依据迟早会和实际问的问题对不上, 而这种错位几乎无法人工发现。

**降级必须可见。** 没配大模型就用规则评分器, 没配嵌入模型就用词面相似度,
没配容器就用本机执行 —— 但这三种降级都会在响应、日志与自检页面里
**明确标注**。一个不说自己降级了的系统, 比一个直接报错的系统更危险。

---

## 6. 主要接口

管理侧(API Key + 角色权限):

| 方法 | 路径 | 权限 |
|---|---|---|
| `POST/GET/PATCH` | `/api/v1/jobs[/{id}]` | `job:write` / `recruit:read` |
| `GET/POST` | `/api/v1/candidates[/{ref}]` | `recruit:read` / `candidate:write` |
| `GET/POST/PATCH` | `/api/v1/applications[/{id}]` | `recruit:read` / `candidate:write` |
| `GET/POST/PATCH/DELETE` | `/api/v1/questions[/{id}]` | `recruit:read` / `question:write` |
| `GET` | `/api/v1/retrieval/search?q=` | `recruit:read` |
| `GET/POST` | `/api/v1/schedules` | `recruit:read` / `schedule:write` |
| `POST` | `/api/v1/schedules/{id}/start` | `session:create` |
| `GET` | `/api/v1/sessions[/{id}][/report]` | `report:read` |
| `POST` | `/api/v1/sessions/{id}/override` | `score:override` |
| `POST` | `/api/v1/sessions/{id}/observer-ticket` | `interview:observe` |
| `GET/DELETE` | `/api/v1/sessions/{id}/recordings[/{kind}]` | `recording:read` / `data:erase` |
| `POST` | `/api/v1/code/run` | `code:run` |
| `GET/POST/DELETE` | `/api/v1/keys[/{id}]` | `key:admin` |
| `GET` | `/api/v1/audit` · `/api/v1/analytics/pipeline` | `audit:read` / `analytics:read` |
| `GET` | `/api/v1/system/providers[?live=1]` | `key:admin` |
| `GET` | `/api/v1/candidates/{ref}/export` · `DELETE /api/v1/candidates/{ref}` | `data:erase` |

候选人侧(一次性会话令牌, 只能访问自己的那一场):

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/v1/candidate/session` | 本场信息(轮次、形态、是否需确认授权) |
| `POST` | `/api/v1/candidate/consent` | 本人确认录音与 AI 评分授权(缺一不可) |
| `POST` | `/api/v1/candidate/resume` | 上传简历, 解析为带原文位置的实体 |
| `POST` | `/api/v1/candidate/events` | 反作弊风险事件(只记录, 不判定) |
| `POST` | `/api/v1/candidate/recording/chunks` · `/finalize` | 录像分片上传与合并 |
| `POST` | `/api/v1/candidate/code/run` | 面试中运行自己的代码 |
| `GET` | `/api/v1/candidate/report` | 自己的报告、授权留痕、简历实体 |
| `WS` | `/ws/interview/{id}?token=` | 文本 / 音频 / 视频信令 |
| `WS` | `/ws/interview/{id}?role=observer&ticket=` | 人类面试官旁听席 |

运维端点: `GET /healthz`(进程存活) · `GET /readyz`(存储可用, 失败返回 503) ·
`GET /metrics`(Prometheus)。

---

## 7. 测试

```bash
make test              # 单元测试(默认后端)
make test-race         # 带竞态检测
make test-mysql        # 存储契约测试: 内存实现与 MySQL 实现跑同一套用例
make frontend-check    # 前端 ES 模块语法检查(前端没有构建步骤, 因此需要显式校验)
make lint              # golangci-lint(需要先安装)
make loadtest          # 并发压测: 同时开 10 场完整面试
```

测试里有几个刻意的选择:

- **沙箱测试跑真实进程**: 超时、输出截断、退出码这些行为, 用 mock 验证等于什么都没验证。
- **存储契约测试同时跑内存与 MySQL**: "本地用内存跑通、线上换 MySQL"如果只靠人自觉,
  迟早会因为某个实现少了一条约束而出现语义差异。
- **前端在 Node 里跑一遍启动路径与全部工作台视图**: 前端没有编译器兜底,
  因此用最小 DOM 存根把"路由没匹配上""某个视图渲染就抛异常"这类问题挡在合并之前。
- **RAG 的追问决策有独立用例**: 断言"漏了要点时必须产生追问、要点答全时必须不追问"。

---

## 8. 部署

```bash
docker build -t ai-interview-platform .          # 多阶段构建 + distroless 运行镜像
kubectl apply -f deployments/k8s/interviewd.yaml # Deployment / HPA / PDB / Ingress
```

K8s 清单里有三处是专门为"长连接面试"调的, 不是模板默认值:

- `terminationGracePeriodSeconds: 120` + `preStop`: 45 分钟的面试不能因为滚动更新被切断。
- Ingress 关闭 `proxy-buffering` 并把读写超时拉到 3600 秒: 否则 AI 的语音会被网关攒着发, 追问听起来像结巴。
- HPA 的缩容稳定窗口 600 秒: 一个 Pod 上可能挂着几十条进行中的长连接。

判题沙箱应当独立部署(见清单里的注释): 面试服务是无状态的、能随便扩容;
判题执行器需要容器运行时权限并且会跑不可信代码。把两者放一起, 等于让
"执行任意代码"发生在承载所有租户会话的进程旁边。

---

## 9. 目录结构

```
cmd/interviewd/        服务入口: 离线模拟 / Web 服务 / 联调自检 / 容器健康检查
internal/api/          HTTP + WebSocket 接入层(鉴权 · 审计 · 视频信令 · 录制 · 判题)
internal/orchestrator/ 面试编排: 阶段状态机 + 问题 DAG + 预算 + 追问策略
internal/knowledge/    题库与 RAG 知识库(提问 / 追问 / 检索三者共用一份索引)
internal/rag/          BM25 + 向量索引 + RRF 融合 + 精排
internal/scoring/      rubric 评分 · 双模型交叉 · 仲裁 · 降级链
internal/resume/       简历解析与原文定位
internal/sandbox/      隔离判题(容器 / 本机, 并如实标注隔离等级)
internal/recording/    录像分片存储与合并
internal/media/        VAD · 流式 ASR/TTS · 打断级联取消
internal/store/        内存 / MySQL 存储(同一套行为契约) + schema.sql
internal/auth/         API Key · 角色权限矩阵 · 候选人会话令牌
internal/privacy/      假名化与脱敏
internal/observability/ 指标与链路追踪
internal/platform/     日志 · goroutine panic 兜底
web/                   零构建前端(index.html + style.css + js/* ES 模块)
deployments/           K8s 清单 · Prometheus 配置 · 压测脚本
docs/                  设计文档与示例报告
```

---

## 10. 数据与合规

- 候选人原始 ID **不落库**, 只落不可逆的 HMAC 假名引用值(`candidate_ref`)。
- 联系方式(邮箱/手机号)在落库前脱敏; 来源 IP 脱敏到网段、UA 截断。
- 开始面试前需要明示同意; 由招聘方后台发起的面试, **必须由候选人本人在面试间确认**,
  否则 WebSocket 不会开始面试(未获同意就录音或评分不是技术瑕疵, 是违规处理个人信息)。
- 审计日志**只追加、不随候选人数据删除** —— 它是平台自身的合规证据。
- 支持数据主体权利: 按 `candidate_ref` 导出全部数据、或删除(录制内容与元数据一并删除)。

更完整的设计取舍见 [docs/设计文档.md](docs/设计文档.md), 示例报告见
[docs/sample-report.json](docs/sample-report.json)。
