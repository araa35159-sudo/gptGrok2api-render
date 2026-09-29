# Render 免费实例与 GitHub 数据备份

这个配置只部署 Go 网关。生图由上游完成；自动注册、Captcha Solver、Redis 和独立图片队列网关不包含在免费实例部署中。

## 1. 准备 GitHub 数据仓库

在自己的 GitHub 账号下另建一个**私有**仓库，例如 `gptgrok2api-data`，初始化 `main` 分支。数据仓库必须与部署源码仓库分开，避免备份提交触发 Render 重新部署。

创建仅对 `araa35159-sudo/gptgrok2api-data` 有访问权的 fine-grained personal access token，授予 **Contents: Read and write**。生成 32 字节加密密钥：

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
```

将密钥和令牌保存在密码管理器中。备份文件虽已加密，GitHub 仓库仍应保持私有。

## 2. 部署 Render Blueprint

从源码仓库创建 Render Blueprint，使用根目录的 `render.yaml`。填写以下变量：

| 变量 | 内容 |
| --- | --- |
| `GITHUB_BACKUP_TOKEN` | 上一步创建的 fine-grained token |
| `GITHUB_BACKUP_KEY` | 上一步生成的 Base64 密钥 |

`CHATGPT2API_AUTH_KEY` 和 `CHATGPT2API_ADMIN_KEY` 由 Blueprint 分别生成。首次部署后，在 Render Dashboard 中查看并妥善保存。保持这两个值和 `GITHUB_BACKUP_KEY` 不变；修改加密密钥后，旧备份无法解密。

`GITHUB_BACKUP_REPO` 已在 Blueprint 中设为 `araa35159-sudo/gptgrok2api-data`，不需要重复填写。

图片 URL 默认使用 Render 提供的 `RENDER_EXTERNAL_URL`。如需自定义域名，可额外设置 `GO_PUBLIC_BASE_URL` 覆盖。

## 3. 数据同步行为

- 服务启动时先确认数据仓库可访问，再读取、解密和恢复快照；GitHub 访问失败或备份损坏时，服务停止启动，避免用空账号池覆盖已有数据。
- 运行中每 30 秒检查一次 JSON 数据，只有内容变化才提交新的加密快照。GitHub 请求失败会在下一轮重试。正常终止时再同步一次。
- 同步包括账号、API 密钥、配置、OAuth、注册和少量任务元数据 JSON。GitHub 仓库中只有一个加密文件 `render-state.enc.json`，账号令牌不会以明文提交。
- 当前是**单实例方案**。不要同时让两台服务器写入同一数据仓库；两个实例各自的本地状态可能互相覆盖。
- 突然断电或强制终止时，最后一次成功同步之后的变更可能丢失，最长通常为约 30 秒。需要更强一致性时应使用数据库。

`data/files/` 内的生成图片不包含在 GitHub 备份中。客户端应及时下载生成图片；如果要保留图库和稳定图片 URL，需要接入对象存储。

当前 Go 版使用 `GITHUB_BACKUP_*` 变量。旧 `.env.example` 中的 `STORAGE_BACKEND=git` 和 `GIT_*` 是另一套旧配置，不能启用这里的备份功能。
