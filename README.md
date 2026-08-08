# ClashRulePilot

个人 OpenClash/Mihomo 规则库与 Telegram 管理 Bot。项目源码位于 `D:\code\myproxy`，Bot 自动维护 GitHub 或 GitLab 公开规则仓库，供 OpenClash 通过 Raw URL 使用。

## 功能

- 定时列出 `Aethersailor/Custom_OpenClash_Rules/rule`，只下载当前的 `*_Domain.yaml`，并额外下载 GEOSITE:CN、GEOSITE:GFW 建立本地查询索引；不拉取整个仓库、`.mrs`、IP 或端口规则。
- 支持从域名、URL、`host:port` 和完整 OpenClash/Mihomo 日志智能提取目标域名。
- 查询个人规则、Aethersailor、GEOSITE:CN、GEOSITE:GFW、DNS 和大陆 IP 信号；本地返回 Fake-IP 时自动通过公网 DoH 获取真实地址。
- 个人规则启动时同步到 `/app/data/personal_rules.cache.json`，查询优先使用本地缓存；GitLab/GitHub 短暂 EOF 时不会阻塞上游规则、DNS 和 GeoIP 查询，缓存每次 Bot 提交成功后原子更新。
- 上游代理/直连规则可通过 Telegram 双向覆写为个人规则。
- Telegram 私聊白名单默认只有 `538031590`。
- 添加规则时智能选择精确域名、当前域名及下级或安全主域名；高级菜单支持关键词、通配符和正则，冲突时二次确认移动。
- 删除个人规则和将已有规则在直连/代理分组之间移动时，Bot 会先列出受影响规则并要求二次确认；重复点击不会重复提交。
- GitHub Trees API 或 GitLab Commits API 原子提交，避免多文件半更新。

外部服务协议优先使用成熟开源库：Telegram 使用 `go-telegram/bot`，国家代码与国旗使用 `biter777/countries`，中文国家名使用 `golang.org/x/text`，GitHub 使用 `google/go-github`，GitLab 使用官方 `api/client-go`，YAML 使用 `goccy/go-yaml`，GeoIP HTTPS 使用你的 `jwwsjlm/req/v3`，其他下载重试使用 `go-retryablehttp`。完整版本和许可证见 [docs/open-source-dependencies.md](docs/open-source-dependencies.md)。

## 启动

1. 复制 `.env.example` 为 `.env`，填写 Telegram Token 以及 GitHub 或 GitLab Token。
2. Docker Compose 默认将同级 `./data` 映射到 `/app/data`，索引可以直接从宿主机查看。
3. 执行 `docker compose pull && docker compose up -d`。
4. 查看 `docker compose logs -f clashrulepilot`，确认仓库初始化、索引同步和 Telegram polling 均成功。

容器启动时只使用 root 对 `DATA_DIR` 自动修正所有权，随后立即降权为 UID/GID `65532` 再启动 GitHub、Telegram、定时同步和健康检查。默认 `./data:/app/data` 不需要手动执行 `chmod` 或 `chown`，也不会修改父目录、`.env` 或 Compose 文件。

新版默认 `DATA_DIR=/app/data`。为兼容旧部署，如果仍配置为 `/data` 且根文件系统只读，程序会自动切换到 `/app/data`；仍建议在 `.env` 中更新为新路径，避免产生兼容提示。

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
DNS_DOMESTIC_ENABLED=true
DNS_DOMESTIC_URLS=https://dns.alidns.com/resolve,https://doh.pub/dns-query
DNS_FOREIGN_ENABLED=true
DNS_FOREIGN_URLS=https://cloudflare-dns.com/dns-query,https://dns.google/resolve
DNS_TIMEOUT=4s
DNS_CACHE_SIZE=2048
DOH_ENABLED=true
DOH_API_URLS=https://cloudflare-dns.com/dns-query,https://dns.google/resolve
DOH_TIMEOUT=4s
DOH_CACHE_SIZE=2048
```

`SYNC_UPSTREAM=false` 只表示不向个人公开仓库镜像 Aethersailor 文件；本地查询索引仍会按 `SYNC_CRON` 更新。每轮同步都以远端当前文件清单完整重建索引，远端已删除或改名的文件会从新索引消失。原始域名规则、`GEOSITE_CN.yaml` 和 `GEOSITE_GFW.yaml` 落地到宿主机 `./data/upstream/`，查询索引保存为 `./data/upstream-index.db`。查询通过 bbolt 按需读取磁盘，不再把完整规则树常驻 Go 堆内存；同步失败时继续使用上一次完整成功数据库。

国内/国外 DNS 查询只用于 Bot 查询、地域判断和规则建议，不会修改 OpenClash/Mihomo 正在使用的 DNS 配置。如果 OpenClash/Mihomo 使用 `fake-ip` DNS 模式，容器查询可能得到 `198.18.0.0/15` 或 `fdfe:dcba:9876::/64` 中的合成地址。程序会并发查询国内组（阿里云 `/resolve`、腾讯 DNSPod）和国外组（Cloudflare、Google），每组主端点失败后切备用端点，再将去重后的真实地址交给 GeoIP。结果按 DNS TTL 缓存，Telegram 查询消息提供国内/国外摘要和逐 IP 详情按钮。旧 `DOH_*` 变量仍兼容；未设置 `DNS_FOREIGN_URLS` 时使用 `DOH_API_URLS`。

## 域名匹配方式

- `DOMAIN`：只匹配完整域名，范围最小，不包含下级域名。
- `DOMAIN-SUFFIX`：匹配填写的域名及其所有下级，适合网站和 CDN；选择主域名时范围较大。
- `DOMAIN-KEYWORD`：域名包含关键词即命中，灵活但容易误匹配。
- `DOMAIN-WILDCARD`：支持 `*`、`?`，比关键词可控；`*.example.com` 通常不包含根域名。
- `DOMAIN-REGEX`：支持复杂正则，能力最强但最难维护。
- `GEOSITE`：维护好的分类数据，依赖数据库更新。
- `RULE-SET`：引用批量规则集合，存在远程依赖和更新延迟。

Bot 默认只展示安全的 `DOMAIN` 和两种 `DOMAIN-SUFFIX` 范围，高级类型放在独立菜单并要求二次确认。规则主域名使用严格 Public Suffix List 计算，避免把动态 DNS 租户规则扩大到整个共享后缀。

## OpenClash 接入

公开规则仓库创建后，在 **服务 → OpenClash → 覆写设置 → 覆写模块** 中订阅 `openclash/personal-overwrite.ini` 的 Raw 地址，类型选择远程/HTTP，目标配置选择“所有配置文件”。

该文件通过 `[YAML]` 的显式 `+rules` 把每条个人规则插入订阅规则之前，并按“精确规则、深层后缀、浅层后缀、通配符、关键词、正则”排序。这样相反动作的精确子域名可以作为主域名规则的例外。OpenClash 中原有 Aethersailor 覆写无需删除。

OpenClash 专项行为规范见 [docs/openclash-guide.md](docs/openclash-guide.md)。

## 许可证与上游署名

上游规则来自 `Aethersailor/Custom_OpenClash_Rules`。分发规则时请同时保留第三方署名和许可证说明。
