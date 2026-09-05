# 图片任务协议

`gpt-image-2` 可能需要数分钟才能完成。图片网关通过 Redis 保存任务，让客户端或 Cloudflare 断开连接后任务仍可继续。

## 创建任务

兼容接口：

```http
POST /v1/images/generations
POST /v1/images/edits
```

显式异步接口：

```http
POST /v1/image-tasks/generations
POST /v1/image-tasks/edits
```

兼容接口会先同步等待。超过 `IMAGE_GATEWAY_SYNC_WAIT_SECS` 后返回 HTTP `202`：

```json
{
  "id": "<task-id>",
  "status": "running",
  "status_url": "/v1/image-tasks/<task-id>",
  "attempts": 1
}
```

响应同时包含 `Location` 和 `Retry-After`。`202` 只表示任务已经进入后台处理，不表示图片生成成功。

## 查询任务

```bash
curl https://api.example.com/v1/image-tasks/<task-id> \
  -H "Authorization: Bearer $API_KEY"
```

- `queued`、`running`：按照 `Retry-After` 或 2-5 秒间隔继续查询。
- `success`：读取 `result.data`，结构与 OpenAI 图片成功响应一致。
- `failed`：读取 `status_code` 和 `error`，不要继续轮询。
- `canceled`：任务被服务端取消。

网关只有在响应包含非空 `data` 数组时才记录成功。上游 `202` 或空响应不会再被包装成成功结果。

## 状态码

| 状态码 | 处理建议 |
| --- | --- |
| `202` | 保存任务 ID 并轮询 |
| `400` | 修正模型、尺寸、提示词或编辑文件 |
| `429` | 队列已满或上游限流，稍后重试 |
| `502` | 上游未产生有效图片，检查任务错误和调用日志 |
| `503` | Redis、代理或上游容量不足；检查 `Too many open connections` |

## Cloudflare 和直连

Cloudflare 的代理连接可能在长任务完成前中断，因此域名入口应把同步等待设为约 90 秒并支持 `202` 轮询。直连 Nginx 入口可将 `proxy_read_timeout` 设置为 900 秒，以兼容只接受最终响应的旧客户端。

即使直连可以长时间等待，客户端仍建议实现异步协议。HTTP 超时只控制连接寿命，不能提高上游生成速度或代理容量。

## 高并发容量

每个账号默认只运行一个图片任务。`IMAGE_GATEWAY_WORKERS`、`GO_IMAGE_MAX_CONCURRENCY` 和代理节点连接上限必须一起规划。1000 个 worker 不代表一个代理可以承载 1000 条连接。

参考图上传遇到超时、EOF、连接重置或 `Too many open connections` 时会有限重试并切换稳定节点。持续出现容量错误时，应降低并发或增加真实可用的代理出口。
