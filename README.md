# GPTGrok2API Go

GPTGrok2API Go 是一个自托管的 OpenAI 兼容网关，使用 Go 运行时接入 OpenAI/ChatGPT JWT 账号池、Grok SSO/OAuth 账号池和代理出口，并提供 Web 控制台、账号调度、图片存储、实时监控与日志管理。

当前版本：`1.2.4-go`

## 能力

- OpenAI 兼容的 Chat Completions、Responses、Anthropic Messages。
- GPT 文本对话、`gpt-image-2` 图片生成和图片编辑。
- Grok 聊天、Responses、图片、图片编辑和视频。
- 账号池、限流冷却、失败反馈、代理绑定和并发调度。
- 请求失败时按状态码排除异常账号并切换账号重试，成功后才向下游返回结果。
- OAuth 账号支持使用 `refresh_token` 刷新并持久化新的 access token；密码和 2FA Secret 不会被当作自动登录凭据。
- 图片本地存储、公开下载 URL、图片管理和批量清理。
- 实时显示入口排队、账号等待、出口代理、上游准备、生成、下载和总耗时。
- 代理订阅支持纯文本和 Base64，自动去重并保留手工节点。
- 代理组支持并发批量检测，持久化节点状态、HTTP 状态码、延迟和错误信息。
- Vue 管理控制台、Redis 队列、健康检查和 Docker Compose 部署。

## Docker 部署

要求 Docker Engine 24+、Docker Compose v2、4 GB 以上可用内存，以及可访问 ChatGPT/Grok 的网络出口。

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git
cd gptGrok2api
cp .env.example .env
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
~~~

在 `.env` 中至少设置：

~~~dotenv
CHATGPT2API_AUTH_KEY=change-this-api-key
CHATGPT2API_ADMIN_KEY=change-this-admin-key
CHATGPT2API_GO_PORT=8000
GO_PUBLIC_BASE_URL=http://your-server-ip:8000
GO_VERSION=1.2.1-go
~~~

启动 Go 版：

~~~bash
docker compose -f docker-compose.go.yml up -d --build
docker compose -f docker-compose.go.yml ps
curl -fsS http://127.0.0.1:8000/health
~~~

Compose 包含 Go API、Redis 和图片网关。主 API 端口由 `CHATGPT2API_GO_PORT` 映射，图片网关默认只监听本机 3001。

~~~bash
docker logs -f gptgrok2api-go
docker compose -f docker-compose.go.yml logs -f image-gateway
~~~

## API

所有接口默认使用 Bearer 认证：

~~~bash
export API_KEY='your-api-key'
curl http://127.0.0.1:8000/v1/models \
  -H "Authorization: Bearer $API_KEY"
~~~

文本聊天：

~~~bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}'
~~~

### GPT 图片聊天

`gpt-image-2` 可通过 `/v1/chat/completions` 调用。纯文本提示词直接使用字符串；只有 `image_url`、`input_image` 或 `image` 内容块会进行图片/Base64 解析。

~~~bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","messages":[{"role":"user","content":"画一个蓝色方块"}]}'
~~~

返回图片链接形如：

~~~text
http://your-server:8000/v1/files/image?id=<image-id>
~~~

该链接由 Go 服务直接提供下载，不依赖上游临时链接。

Go 版会同时识别 ChatGPT 图片流程返回的 `file-service://` 和 `sediment://` 文件引用，使用对应会话和附件 ID 下载结果，然后写入本地图片存储并返回本站 `/v1/files/image` URL。`sediment://` 不会作为不可下载文本继续轮询，因此可避免结果已经生成但最终仍报 `OpenAI image result polling timed out`。

### 图片生成和编辑

~~~bash
curl http://127.0.0.1:8000/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"画一只猫","size":"1024x1024"}'
~~~

#### 异步任务和状态码

图片生成通常需要数分钟。`POST /v1/images/generations` 在同步等待窗口内拿到图片时返回 `200`；如果请求经过 Cloudflare 或其他边缘网关，网关会在约 90 秒后返回 `202`，任务不会停止。响应中的 `id`、`status_url` 和 `Location` 用于轮询：

~~~bash
curl http://127.0.0.1:8000/v1/image-tasks/<task-id> \
  -H "Authorization: Bearer $API_KEY"
~~~

轮询结果的 `status` 为 `queued`/`running` 时继续等待；`success` 时读取 `result.data`；`failed` 时读取 `status_code` 和 `error`。常见状态码含义如下：

| 状态码 | 含义 |
| --- | --- |
| `202` | 任务已接受并继续后台执行，不代表成功或失败 |
| `400` | 请求参数错误，例如模型不支持或图片尺寸无效 |
| `502` | 上游没有返回有效图片，或网关收到上游 `202`/空结果 |
| `503` | 队列、Redis 或代理出口暂时不可用 |

直连服务器的 `:3000` 端口不经过 Cloudflare，默认最多等待 900 秒；公网域名建议始终实现 `202` 轮询。终态任务默认在 Redis 保留 24 小时，避免图片已生成但轮询稍晚导致任务消失。

#### 图片尺寸兼容

OpenAI 图片接口接受标准尺寸以及常见比例写法。`1:1`、`2:3`、`3:2`、`3:4`、`4:3`、`9:16`、`16:9` 会在网关中转换为上游要求的 16 像素对齐尺寸；`1024x1365`、`1920x1080` 等旧前端值也会自动校正。无法识别的尺寸会明确返回 `400 invalid image size`。

图片编辑必须使用 `multipart/form-data`：

~~~bash
curl http://127.0.0.1:8000/v1/images/edits \
  -H "Authorization: Bearer $API_KEY" \
  -F 'model=gpt-image-2' \
  -F 'prompt=把背景改成蓝色' \
  -F 'image=@input.png;type=image/png'
~~~

### 主要路由

| 功能 | 方法和路径 |
| --- | --- |
| 健康检查 | `GET /health` |
| 模型列表 | `GET /v1/models` |
| 对话补全 | `POST /v1/chat/completions` |
| Responses | `POST /v1/responses` |
| Anthropic Messages | `POST /v1/messages` |
| 图片生成 | `POST /v1/images/generations` |
| 图片编辑 | `POST /v1/images/edits` |
| 图片下载 | `GET /v1/files/image?id=...` |
| 实时监控 | `GET /api/monitor/realtime` |
| 日志管理 | `GET /api/logs` |
| 图片管理 | `GET /api/images` |
| 代理订阅刷新 | `POST /api/proxy/groups/{id}/subscription/refresh` |
| 代理组节点检测 | `POST /api/proxy/groups/test` |
| 账号异常清理预览 | `POST /api/settings/account-cleanup/preview` |
| 账号异常清理执行 | `POST /api/settings/account-cleanup/run` |
| 管理控制台 | `GET /` |

管理路由需要 `CHATGPT2API_ADMIN_KEY`。

## 账号策略

### 失败换号重试

`/v1/chat/completions`、`gpt-image-2` 图片聊天和 `/v1/images/generations` 在上游返回 `401`、`403`、`429`、`500`、`502`、`503` 或网络错误时，会将当前账号加入本次请求的排除列表并尝试下一个账号。只有在重试耗尽或没有可用账号时，才向下游返回错误。流式请求已经发送首个 SSE 事件后才发生断流时，无法再无感切换账号。

### AT 刷新

带 `refresh_token` 的 OAuth 账号可以通过账号刷新接口获取新的 access token，并自动写回 `data/accounts.json`。普通请求遇到过期 AT 时会优先执行请求级重试；当前 Go 版不会使用账户密码或 2FA Secret 自动重新登录生成 AT，没有 `refresh_token` 的过期账号需要重新导入或人工处理。

### 自动移除异常账号

在控制台勾选“自动移除异常账号”后，系统会先执行预览。只有被明确标记为认证失效/过期且没有 `refresh_token` 的账号才会进入异常删除候选；有刷新令牌的账号和临时网络错误会保留。确认“立即移除”后才执行删除，预览不会修改账号数据。请求过程中上游明确返回 `401`/`403` 时，也会在状态写入后自动执行同样的保守清理；`502`/`503`、超时和图片下载失败不会触发删除。

账号编辑弹窗中的“账户密码”和“2FA Secret”支持点击“复制”；字段为空时复制按钮会自动禁用。

## 代理订阅与节点检测

在控制台的代理组中填写订阅 URL 后，可以直接刷新订阅。Go 服务会真实请求该地址，支持以下内容：

- 每行一个 `http://`、`https://`、`socks4://`、`socks5://` 或 `socks5h://` 节点。
- 没有协议的 `host:port`，按 HTTP 代理导入。
- Base64 编码的上述纯文本代理列表。
- 自动规范化和去重，单次最多导入 5000 个订阅节点。
- 刷新时替换旧的订阅节点，同时保留管理员手工添加的节点。
- 请求、解析或并发修改失败时，保留旧配置并记录最近刷新时间和错误。

“检测全部节点”会以最多 32 路并发真实经过每个代理访问 ChatGPT 探测地址。只有 `2xx` 和 `3xx` 判定为可用，`403`、其他 `4xx/5xx`、连接失败和超时均判定为不可用。每个节点的检测时间、延迟、HTTP 状态和错误会保存到配置，刷新页面后仍可查看。

实时监控的活跃请求会显示本次请求实际租用的出口代理；直连、代理组节点和备用出口会分别标识，便于根据同一请求 ID 排查失败链路。

## Go 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CHATGPT2API_AUTH_KEY` | 无 | API 认证密钥 |
| `CHATGPT2API_ADMIN_KEY` | 无 | 管理密钥 |
| `CHATGPT2API_GO_PORT` | `3000` | 宿主机端口 |
| `GO_LISTEN_ADDR` | 程序默认 `:8080` | 监听地址；Go Docker 镜像固定为容器内 `:80` |
| `GO_PUBLIC_BASE_URL` | 空 | 图片公开 URL 基础地址 |
| `GO_CONFIG_PATH` | `/app/data/config.json` | 配置文件路径 |
| `GO_ACCOUNTS_PATH` | `data/accounts.json` | OpenAI 账号文件 |
| `GO_AUTH_KEYS_PATH` | `data/auth_keys.json` | 用户密钥文件 |
| `GO_REQUEST_TIMEOUT_SECONDS` | `180` | 上游超时，最大 300 秒 |
| `GO_CHAT_MAX_RETRIES` | `2` | 聊天/图片最大重试次数 |
| `GO_IMAGE_ACCOUNT_CONCURRENCY` | `1` | 单个 ChatGPT 账号同时执行的图片任务数（1-4） |
| `GO_IMAGE_MAX_CONCURRENCY` | `128` | 单进程同时执行的 ChatGPT 图片任务总数（1-1024） |
| `GO_CHAT_RETRY_CODES` | `401,403,429,500,502,503,504` | 触发换号重试的上游 HTTP 状态码 |
| `GO_QUEUE_BACKEND` | `redis` | `redis` 或 `json` |
| `GO_REDIS_ADDR` | `redis:6379` | Redis 地址 |
| `GO_PROXY_URL` | 空 | 默认代理 |
| `GO_PROXY_POOL` | 空 | 逗号分隔代理池 |
| `GO_OPENAI_BASE_URL` | `https://chatgpt.com` | ChatGPT 上游 |
| `GO_OPENAI_LOGIN_SERVICE_URL` | 空 | 无 RT 账号的内部网页登录 AT 刷新桥接地址 |
| `GO_OPENAI_LOGIN_SERVICE_KEY` | 空 | 内部网页登录桥接共享密钥，不应暴露给客户端 |
| `GO_OPENAI_LOGIN_CONCURRENCY` | `3` | 无 RT 浏览器登录并发，范围 `1-4` |
| `GO_VERSION` | `1.2.5-go` | 版本标识 |
| `GO_IMAGE_RETENTION_DAYS` | `1` | 本地图片和元数据保留天数 |
| `GO_IMAGE_CLEANUP_INTERVAL_SECONDS` | `3600` | 自动清理检查间隔，最少 60 秒 |

完整示例见 `.env.example` 和 `config.example.yaml`。

## 服务器 8000 端口

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git /opt/gpt2api-go
cd /opt/gpt2api-go
cp .env.example .env
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
docker compose -f docker-compose.go.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

服务器配置示例：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_GO_PORT=8000
GO_PUBLIC_BASE_URL=https://gpt.qkmss.com
GO_VERSION=1.2.4-go
~~~

更新前备份 `/opt/gpt2api-go/data`，更新后检查 `/health`、`/v1/models` 和 `/v1/files/image?id=...`。

### 图片网关高并发参数

`docker-compose.go.yml` 已包含 Redis 图片队列和独立网关。1000 个账号的示例参数为 `IMAGE_GATEWAY_WORKERS=1000`、`IMAGE_GATEWAY_QUEUE_CAPACITY=5000`、`IMAGE_GATEWAY_BACKEND_TIMEOUT_SECS=900`、`IMAGE_GATEWAY_SYNC_WAIT_SECS=90`。实际并发必须小于代理服务商的连接容量；出现 `503 Too many open connections` 时应降低 `GO_IMAGE_MAX_CONCURRENCY` 或增加代理出口。上传阶段会进行有限次数的代理切换重试，但不会突破代理方的连接上限。

## 本地开发

Go 版不需要 Python 或 Uvicorn：

~~~bash
go run ./cmd/gptgrok2api
go test ./internal/...
CGO_ENABLED=0 go build -trimpath -o gptgrok2api ./cmd/gptgrok2api
cd go-image-gateway && go test ./...
cd ..
docker compose -f docker-compose.go.yml config
~~~

前端源码位于 `web-vue/`，Docker 构建时会自动生成 `web_dist/`。

## 数据安全

以下运行时数据不会提交到 Git：

~~~text
.env
config.json
data/
logs/
~~~

`data/` 可能包含账号 Token、Cookie、OAuth 凭据、图片和管理密钥。Go 版默认把生成结果及其元数据保留 1 天，后台每小时自动清理过期文件；可通过 `GO_IMAGE_RETENTION_DAYS` 调整。生产环境请使用随机密钥，不要上传运行时数据，只通过 Nginx/HTTPS 暴露 API，并定期备份 `data/`。

## 开源贡献

提交问题时请附上版本号、请求路径、任务 ID、轮询结果中的 `status_code` 和脱敏后的错误信息；不要提交 API Key、Cookie、JWT、代理凭据、账号文件或生成图片。功能修改应至少通过 `go test ./...`，涉及网关时在 `go-image-gateway/` 目录运行 `go test ./...`。

## 许可证

请保留 `LICENSE` 和 `GROK2API_LICENSE`。项目基于 [yukkcat/chatgpt2api](https://github.com/yukkcat/chatgpt2api) 开发，并包含 GPTGrok2API Go 自有修改。
