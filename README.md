# ClashRulePilot

个人 OpenClash/Mihomo 规则库与 Telegram 管理 Bot。项目源码位于 `D:\code\myproxy`，Bot 自动维护 GitHub 或 GitLab 公开规则仓库，供 OpenClash 通过 Raw URL 使用。

## 功能

- 定时列出 `Aethersailor/Custom_OpenClash_Rules/rule`，只下载当前的 `*_Domain.yaml` 与 GEOSITE:CN 并建立本地查询索引；不拉取整个仓库、`.mrs`、IP 或端口规则。
- 支持从域名、URL、`host:port` 和完整 OpenClash/Mihomo 日志智能提取目标域名。
- 查询个人规则、Aethersailor、GEOSITE:CN、DNS 和大陆 IP 信号。
- 上游代理/直连规则可通过 Telegram 双向覆写为个人规则。
- Telegram 私聊白名单默认只有 `538031590`。
- 添加规则时选择直连/代理、精确域名/包含子域名；冲突时二次确认移动。
- GitHub Trees API 或 GitLab Commits API 原子提交，避免多文件半更新。

外部服务协议优先使用成熟开源库：Telegram 使用 `go-telegram/bot`，GitHub 使用 `google/go-github`，GitLab 使用官方 `api/client-go`，YAML 使用 `goccy/go-yaml`，GeoIP HTTPS 使用你的 `jwwsjlm/req/v3`，其他下载重试使用 `go-retryablehttp`。完整版本和许可证见 [docs/open-source-dependencies.md](docs/open-source-dependencies.md)。

## 启动

1. 复制 `.env.example` 为 `.env`，填写 Telegram Token 以及 GitHub 或 GitLab Token。
2. Docker Compose 默认将同级 `./data` 映射到 `/app/data`，索引可以直接从宿主机查看。
3. 执行 `docker compose pull && docker compose up -d`。
4. 查看 `docker compose logs -f clashrulepilot`，确认仓库初始化、索引同步和 Telegram polling 均成功。

容器启动时只使用 root 对 `DATA_DIR` 自动修正所有权，随后立即降权为 UID/GID `65532` 再启动 GitHub、Telegram、定时同步和健康检查。默认 `./data:/app/data` 不需要手动执行 `chmod` 或 `chown`，也不会修改父目录、`.env` 或 Compose 文件。

Token 只通过环境变量注入，不要提交到仓库。曾经在聊天、截图或日志中公开过的 Token 必须撤销后重新生成。

GitHub 示例：

```env
RULE_REPO_PROVIDER=github
RULE_REPO_PROJECT=jwwsjlm/clashrulepilot-rules
RULE_REPO_BRANCH=main
GITHUB_TOKEN=新的Token
```

GitLab.com 或自建 GitLab 示例：

```env
RULE_REPO_PROVIDER=gitlab
RULE_REPO_PROJECT=用户名或群组/clashrulepilot-rules
RULE_REPO_BRANCH=main
GITLAB_BASE_URL=https://gitlab.example.com
GITLAB_TOKEN=新的Token
```

## Telegram 命令

Bot 默认使用聊天消息内的 Inline Keyboard，以下命令继续兼容：

```text
/query example.com
/add example.com
/remove example.com
/list
/sync
/status
/help
```

域名输入支持：

```text
subs.example.com
subs.example.com:443
https://subs.example.com/path
[TCP] 192.168.1.2:12345 --> subs.example.com:443 match Match using Proxy
```

## 查询索引

```env
UPSTREAM_INDEX_ENABLED=true
SYNC_CRON=0 3 * * *
DATA_DIR=/app/data
GEOIP_API_URL=https://ipwho.is/{ip}
```

`SYNC_UPSTREAM=false` 只表示不向个人公开仓库镜像 Aethersailor 文件；本地查询索引仍会按 `SYNC_CRON` 更新。每轮同步都以远端当前文件清单完整重建索引，远端已删除或改名的文件会从新索引消失。宿主机 `./data` 中只保留最新的 `upstream-index.json.gz` 压缩快照，旧式快照与失败遗留的临时文件会被清理；同步失败时继续使用上一次完整成功快照。

## OpenClash 接入

公开规则仓库创建后，在 **服务 → OpenClash → 覆写设置 → 覆写模块** 中订阅 `openclash/personal-overwrite.ini` 的 Raw 地址，类型选择远程/HTTP，目标配置选择“所有配置文件”。

该文件通过 `[YAML]` 的 `+rules` 把 `my_proxy`、`my_direct` 放在订阅规则前面，因此个人直连能够覆盖 Aethersailor 代理规则，个人代理也能覆盖上游直连规则。OpenClash 中原有 Aethersailor 覆写无需删除。

OpenClash 专项行为规范见 [docs/openclash-guide.md](docs/openclash-guide.md)。

## 许可证与上游署名

上游规则来自 `Aethersailor/Custom_OpenClash_Rules`。分发规则时请同时保留第三方署名和许可证说明。
