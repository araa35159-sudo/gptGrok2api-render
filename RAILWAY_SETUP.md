# Railway 部署指南

本仓库使用根目录的 `Dockerfile` 构建单个 Go 网关。Railway 自动提供的 `PORT` 会用于监听地址；生成 Railway 公网域名后，图片链接会自动使用该域名。不要把 `render.yaml` 当作 Railway 配置。

## 1. 创建服务

在 Railway 新建项目，选择 **Deploy from GitHub repo**，连接 `araa35159-sudo/gptGrok2api-render`。只创建一个服务，不添加 Redis、数据库或独立图片队列。仓库根目录的 Dockerfile 会构建前端和 Go 服务。首次部署若因尚未填写变量而失败，填写下述变量后重新部署。

## 2. 配置 Variables

在服务的 **Variables** 中填写：

| 名称 | 值 |
| --- | --- |
| `CHATGPT2API_AUTH_KEY` | 自己生成的长随机 API 密钥 |
| `CHATGPT2API_ADMIN_KEY` | 与 API 密钥不同的长随机后台密钥 |
| `GITHUB_BACKUP_REPO` | `araa35159-sudo/gptgrok2api-data` |
| `GITHUB_BACKUP_TOKEN` | 仅授权上述私有仓库 `Contents: Read and write` 的 fine-grained GitHub token |
| `GITHUB_BACKUP_KEY` | 32 字节随机值的 Base64 编码；后续必须保持不变 |
| `GO_IMAGE_MAX_CONCURRENCY` | `16` |
| `GO_IMAGE_ACCOUNT_CONCURRENCY` | `16` |

总图片并发与每账号并发均设为 16，只有一个可用 ChatGPT 账号时也允许最多同时处理 16 张。修改这两个变量后点击 Railway 的 Deploy 应用；旧服务的 2/1 或 4/2 变量会覆盖代码默认值，更新代码不会自动修改它们。第 17 张及以后的图片会排队等待名额。上游账号仍可能返回 429，并发增加主要缩短本地排队，不会直接缩短单张图片的上游生成时间。

Windows PowerShell 可用以下命令分别生成随机值：

```powershell
[Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

前一条各执行一次，分别用于 API 密钥和后台密钥；后一条用于 `GITHUB_BACKUP_KEY`。将三个值保存在密码管理器。不要把密钥或 GitHub token 写入代码仓库、聊天或截图。

三个 `GITHUB_BACKUP_*` 变量必须一起设置。服务启动时会先从私有仓库恢复加密 JSON，然后每 30 秒检查变化并同步。只运行一个实例，避免并发覆盖。备份仓库已建立，仓库内的默认文件名仍是 `render-state.enc.json`，在 Railway 使用它不影响功能。

## 3. 开启公网地址并检查

部署完成后，在服务 **Settings → Networking → Public Networking → Generate Domain** 创建 `*.up.railway.app` 地址。访问 `https://域名/health`，应返回健康响应；再访问域名根路径打开管理页面。Railway 提供的 `RAILWAY_PUBLIC_DOMAIN` 会用于返回图片链接，无需手填 `GO_PUBLIC_BASE_URL`。如使用自己的域名，可设置 `GO_PUBLIC_BASE_URL=https://自己的域名`。

不要在 Railway 手动设置 `GO_LISTEN_ADDR` 或 `PORT`，除非明确知道服务域名的 target port 如何匹配。若显示 `Application failed to respond`，先检查部署日志中 `listening on :端口`，再检查域名的 target port。

## 存储与费用

GitHub 备份只保存加密 JSON，不保存 `data/files/` 下的图片。默认图片保留一天，客户端应及时下载；重启或重新部署可能使旧图片链接失效。Railway Free 的临时磁盘与内存有限，不建议把它当长期图库。

Railway 当前价格页写明：新账号先有 30 天、5 美元试用额度；随后 Free 计划每月提供 1 美元资源额度，超出额度的全天运行服务不适合长期免费使用。每月约 200 张 × 2 MB × 30 天，单算图片出站约 12 GB，按官方每 GB 0.05 美元的出站价格约 0.60 美元；内存和 CPU 另计。正式长期使用前，在 Railway 的 Usage 页面检查实际消耗和账单设置。Hobby 计划最低每月 5 美元。

价格来源：[Railway Pricing](https://railway.com/pricing)、[Railway Plans](https://docs.railway.com/reference/pricing/plans)。
