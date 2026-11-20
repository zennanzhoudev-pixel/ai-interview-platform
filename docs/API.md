# API 参考

本项目的 HTTP API 全部挂在 `/api/v1` 下, WebSocket 在 `/ws` 下。
所有请求与响应均为 JSON (音频帧与录像分片除外), 错误统一为 `{"error": "..."}`。

## 身份认证

系统有三种身份, 互不混用:

| 身份 | 凭证 | 用途 |
|---|---|---|
| 账号会话 | `HttpOnly` Cookie `ios_session` | 企业成员 / 候选人登录后使用 |
| 候选人会话令牌 | `?token=` 查询参数 | 一次面试链接, 只能访问自己那一场 |
| API Key | `X-API-Key` 请求头 | 机器集成 (ATS 等), 受 RBAC 权限点约束 |

登录成功返回:

```json
{
  "user": {"user_id": "u_...", "name": "...", "role": "admin"},
  "role": "admin",
  "staff": true,
  "expires_at": "2026-11-26T10:00:00Z",
  "next_path": "/console/dashboard"
}
```

`next_path` 由服务端决定, 前端不要自行按角色猜测入口。

## 健康检查与指标

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 进程存活 |
| GET | `/readyz` | 存储可用; 失败返回 503 |
| GET | `/metrics` | Prometheus 指标 |

## 账号接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/auth/register` | 注册 (`name/email/phone/password/role/invite_code`) |
| POST | `/api/v1/auth/login` | 密码登录 (`identifier/password`) |
| POST | `/api/v1/auth/logout` | 退出并清除 Cookie |
| GET | `/api/v1/auth/me` | 当前登录账号 |
| POST | `/api/v1/auth/password` | 修改密码 (`current_password/new_password`) |
| POST | `/api/v1/auth/password/forgot` | 发送找回密码验证码 (`email`) |
| POST | `/api/v1/auth/password/reset` | 重置密码 (`email/code/new_password`) |
| POST | `/api/v1/auth/verify/request` | 发送联系方式验证码 (`kind: email\|phone`, `target`) |
| POST | `/api/v1/auth/verify/confirm` | 校验验证码 (`kind/target/code`) |
| POST | `/api/v1/auth/face/enroll` | 录入人脸 (多帧 `frames[]`, `frames` 间必须有变化) |
| DELETE | `/api/v1/auth/face` | 删除人脸模板 |
| POST | `/api/v1/auth/face/check` | 用一帧检查相似度 (不登录) |
| POST | `/api/v1/auth/face/login` | 人脸登录 (`identifier/frame`) |
| GET | `/api/v1/auth/accounts` | 列出账号 (企业成员) |

验证码规则: 6 位数字、10 分钟有效、最多 5 次尝试。开发模式写日志;
配置 `SMTP_HOST/SMTP_PORT/SMTP_USER/SMTP_PASS/SMTP_FROM` 后走真实邮件。

## 候选人接口

需要 `?token=` 候选人会话令牌。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/candidate/session` | 本场信息 (轮次/形态/是否需授权) |
| POST | `/api/v1/candidate/consent` | 录音与评分授权 (`recording/scoring`) |
| POST | `/api/v1/candidate/resume` | 上传简历并解析 |
| POST | `/api/v1/candidate/events` | 反作弊风险事件 (只记录, 不判定) |
| POST | `/api/v1/candidate/recording/chunks` | 录像分片上传 |
| POST | `/api/v1/candidate/recording/finalize` | 合并分片 |
| POST | `/api/v1/candidate/code/run` | 面试中运行代码 |
| GET | `/api/v1/candidate/report` | 本人报告 / 授权留痕 / 简历实体 |
| GET | `/api/v1/candidate/history` | 本人历史面试 |

## 管理接口

需要 API Key 或企业成员会话, 并具备对应权限点。

### 招聘域

| 方法 | 路径 | 权限 |
|---|---|---|
| GET/POST/PATCH | `/api/v1/jobs[/{id}]` | `recruit:read` / `job:write` |
| GET/POST | `/api/v1/candidates[/{ref}]` | `recruit:read` / `candidate:write` |
| GET/POST/PATCH | `/api/v1/applications[/{id}]` | `recruit:read` / `candidate:write` |
| GET/POST/PATCH/DELETE | `/api/v1/questions[/{id}]` | `recruit:read` / `question:write` |
| GET/POST | `/api/v1/schedules` | `recruit:read` / `schedule:write` |
| POST | `/api/v1/schedules/{id}/start` | `session:create` |

`schedules/{id}/start` 是**员工专属**: 它把排期变成真实面试会话并返回候选人链接。
候选人不要调用这个接口。

### 面试与会话

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/api/v1/sessions[/{id}]` | `report:read` |
| GET | `/api/v1/sessions/{id}/report` | `report:read` |
| POST | `/api/v1/sessions/{id}/override` | `score:override` |
| GET | `/api/v1/sessions/{id}/consents` | `report:read` |
| POST | `/api/v1/sessions/{id}/observer-ticket` | `interview:observe` |
| GET | `/api/v1/sessions/{id}/ai-log` | `report:read` |
| GET/DELETE | `/api/v1/sessions/{id}/recordings[/{kind}]` | `recording:read` / `data:erase` |
| POST | `/api/v1/resume/parse` | `session:create` |
| POST | `/api/v1/code/run` | `code:run` |

### 检索与知识库

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/api/v1/knowledge/stats` | `recruit:read` |
| GET | `/api/v1/retrieval/search?q=` | `recruit:read` |

### 运营与治理

| 方法 | 路径 | 权限 |
|---|---|---|
| GET/POST/DELETE | `/api/v1/keys[/{id}]` | `key:admin` |
| GET | `/api/v1/audit` | `audit:read` |
| GET | `/api/v1/analytics/overview` | `analytics:read` |
| GET | `/api/v1/analytics/pipeline` | `analytics:read` |
| GET | `/api/v1/candidates/{ref}/export` | `data:erase` |
| DELETE | `/api/v1/candidates/{ref}` | `data:erase` |
| GET | `/api/v1/system/providers[?live=1]` | `key:admin` |
| GET | `/api/v1/system/ice` | `recruit:read` |

## 真人双向对练

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/practice/join` | 进入配对队列 |
| POST | `/api/v1/practice/leave` | 离开队列/房间 |
| GET | `/api/v1/practice/room/{id}` | 房间信息 |
| GET | `/api/v1/practice/rooms` | 当前房间列表 |
| WS | `/ws/practice/{id}` | 对练信令 |

配对状态在配置 `REDIS_ADDR` 后走 Redis, 支持多副本; 否则退化为单实例内存。

## WebSocket: 面试间

```
WS /ws/interview/{session_id}?token=<候选人令牌>
WS /ws/interview/{session_id}?role=observer&ticket=<旁听票据>
```

同一条连接承载三种消息:

- **文本**: JSON 消息 (作答、阶段推进、状态事件);
- **音频**: 二进制 PCM16LE 单声道 16kHz 帧 (20ms / 640 字节);
- **视频信令**: JSON 形式的 WebRTC `offer/answer/ice` (媒体本身不过服务端)。

一条连接承载三条链路, 面试间只需要一个端口、一次鉴权、一套重连逻辑。

## 错误码

| 状态码 | 含义 |
|---|---|
| 400 | 参数不合法 |
| 401 | 未登录 / 令牌失效 |
| 403 | 已登录但无权限 |
| 404 | 资源不存在 |
| 409 | 冲突 (如账号已存在) |
| 412 | 前置条件不满足 (如未录入人脸) |
| 429 | 触发限流 / 验证码尝试过多 |
| 503 | 依赖未配置或暂不可用 |

## 示例

```bash
# 密码登录
curl -c cookie.txt -X POST http://localhost:8101/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"identifier":"admin@demo.local","password":"Demo123456!"}'

# 发起找回密码
curl -X POST http://localhost:8101/api/v1/auth/password/forgot \
  -H 'Content-Type: application/json' -d '{"email":"admin@demo.local"}'

# 用 API Key 管理职位
curl http://localhost:8101/api/v1/jobs -H 'X-API-Key: <key>'
```
