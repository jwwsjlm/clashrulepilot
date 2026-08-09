# Open-source dependencies

ClashRulePilot 优先使用持续维护、职责清晰的开源库，避免自行实现外部服务协议。

| 组件 | 固定版本 | 用途 | 许可证 |
|---|---:|---|---|
| `github.com/go-telegram/bot` | `v1.23.0` | Telegram Bot API、long polling、消息、Inline Keyboard、回调确认、API 错误模型 | MIT |
| `github.com/biter777/countries` | `v1.7.5` | ISO 3166 国家代码、标准国旗 Emoji 与国家元数据 | MIT |
| `github.com/google/go-github/v81` | `v81.0.0` | GitHub 用户、仓库、Contents、Git Trees/Blobs/Commits/Refs API | BSD-3-Clause |
| `gitlab.com/gitlab-org/api/client-go` | `v1.46.0` | GitLab 项目、文件、分支和原子 commit `actions[]` | Apache-2.0 |
| `github.com/goccy/go-yaml` | `v1.19.2` | Aethersailor 与 GEOSITE classical YAML 解析 | MIT |
| `github.com/hashicorp/go-retryablehttp` | `v0.7.8` | GitHub 与规则下载的退避重试 | MPL-2.0 |
| `github.com/hashicorp/golang-lru/v2` | `v2.0.7` | 公网 DoH 查询结果的容量限制 LRU 缓存 | MPL-2.0 |
| `github.com/jwwsjlm/req/v3` | `v3.58.5` | GeoIP 和公网 DoH HTTPS 请求、响应解码与退避重试 | MIT |
| `github.com/robfig/cron/v3` | `v3.0.1` | `SYNC_CRON` 定时调度 | MIT |
| `go.etcd.io/bbolt` | `v1.5.0` | 磁盘域名索引、事务更新和按需查询 | MIT |
| `golang.org/x/net` | `v0.57.0` | IDN 和 Public Suffix List | BSD-3-Clause |
| `golang.org/x/sync/singleflight` | `v0.22.0` | 合并相同域名的并发 DNS/DoH/GeoIP 查询，以及同步任务防重 | BSD-3-Clause |

## 设计边界

- Telegram JSON、long polling、更新 offset、429/error parsing 不再由项目自行实现。
- Telegram 更新允许跨聊天并发处理；同一私聊使用聊天级互斥锁，回调 ID 使用短期去重窗口，避免重复提交。
- GitHub/GitLab 的认证、URL 编码、请求/响应模型交给 SDK；项目只保留“多个规则文件组成一次逻辑提交”的业务编排。
- YAML 交给解析器校验，项目只解析受支持的 Mihomo classical 规则类型。
- 域名规则索引由 bbolt 落地到磁盘，Go 内存只保留少量数据库元数据；个人规则优先级、冲突确认和 Telegram 会话属于 ClashRulePilot 业务逻辑。
- `singleflight` 只合并无副作用的网络查询和同步任务；Telegram 会话、按钮状态、规则提交和用户隔离不会共享。
- 所有依赖均在 `go.mod` 固定版本，升级前必须运行完整测试和 Docker 构建。
