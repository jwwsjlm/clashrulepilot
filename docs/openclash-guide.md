# OpenClash 参考与回答规范

这是 ClashRulePilot 的 OpenClash 文档规范，不实现 Telegram Bot 的大模型问答功能。

## 权威参考

回答 OpenClash 问题前，必须获取并完整阅读：

`https://raw.githubusercontent.com/vernesong/OpenClash/dev/.github/skills/openclash-user-guide/SKILL.md`

该指南是完整依赖清单、错误速查、防火墙链、LuCI 配置项、DNS、防泄露和订阅/GEO 流程的主要来源。

## 必须遵循的规则

1. 默认提供 LuCI Web 路径，例如：
   **服务 → OpenClash → 插件设置**。
2. 除非用户明确要求，不优先给出命令行操作。
3. 解释底层原理，包括 fw4/nftables、fw3/iptables 规则链以及 YAML 覆写和转换逻辑。
4. 排障第一步检查依赖：
   - **系统 → 软件包**：确认 `curl`、`ca-bundle`、`dnsmasq-full`、Ruby YAML 组件等；
   - **服务 → OpenClash → 插件设置 → 调试日志 → 生成**：查看完整依赖检查段。
5. 指南没有覆盖的内容，按以下优先级查阅实际资料：
   - Mihomo Wiki；
   - Meta-Docs；
   - OpenClash 源码；
   - Mihomo 核心源码；
   - Smart 核心源码；
   - OpenClash GitHub Issues。
6. 不猜测、不编造；回答必须注明具体章节或外部来源。
7. 问题描述不完整或缺少错误信息时，先要求用户生成调试日志，不直接猜测原因。

## 规则库与 OpenClash 的关系

Bot 仍生成 Mihomo `classical` rule-provider 文件用于兼容和查看，但推荐的 `personal-overwrite.ini` 直接生成显式个人规则：

```yaml
+rules:
  - DOMAIN,cdn.example.com,DIRECT
  - DOMAIN-SUFFIX,example.com,🚀 手动选择
```

个人规则必须排在上游规则之前，否则上游规则可能先命中。显式规则按精确度排序，使更具体的子域名规则能够覆盖较宽的主域名规则。

ClashRulePilot 生成的 `personal-overwrite.ini` 使用 `[YAML]` 和 `+rules` 前置插入。根据权威指南第 8 章，OpenClash 会先下载远程覆写到 `/etc/openclash/overwrite/`，再将覆写内容合并到运行配置。Bot 的本地 Aethersailor 索引只用于查询，不参与路由器防火墙链，也不会改变 nftables/iptables 透明代理规则。

## OpenClash 覆写自检边界

生产环境的推荐分层是：GitHub `jwwsjlm/clashrulepilot` 仅保存源码；GitLab 私有项目 `someme/clashrulepilot-rules-private` 保存个人规则权威源；GitLab 公共项目 `someme/clashrulepilot-rules` 发布 OpenClash 和 Clash/Mihomo 兼容文件。远程覆写使用公共项目地址：

```text
https://gitlab.com/someme/clashrulepilot-rules/-/raw/main/openclash/personal-overwrite.ini
```

公共镜像由 GitHub Actions 每天北京时间 03:00 同步生成，容器保持 `SYNC_UPSTREAM=false`，避免与 Workflow 同时写入上游文件。

Bot 状态页中的 **🧪 OpenClash 覆写自检** 只检查发布侧，主要验证：

- GitHub/GitLab 仓库、目标分支和 Raw 文件是否可访问；
- `data/personal_rules.json`、程序重新渲染结果和 `openclash/personal-overwrite.ini` 是否一致；
- `[YAML]`、`+rules`、UTF-8、YAML 语法、字段数量、去重和规则顺序是否正确；
- 代理策略组名称是否为空，以及是否存在危险的宽泛规则。

首版不会连接 OpenWrt 路由器，因此自检通过只代表“远程覆写文件可以被 OpenClash 下载并解析”，不能证明订阅中确实存在对应策略组，也不能证明路由器已经启用该覆写。

在 LuCI 中确认启用路径：

**服务 → OpenClash → 配置订阅 → 编辑当前订阅 → 远程覆写**

权威指南第 8 章说明：远程覆写会下载到 `/etc/openclash/overwrite/`；`[YAML]` 段中的 `+rules` 会在订阅 YAML 转换过程中追加到运行配置的规则前部。因此个人精确规则和更深层级后缀规则应排在宽泛上游规则之前。

如果发布侧自检全部正常但规则仍未生效：

1. 先到 **系统 → 软件包** 检查 OpenClash 依赖是否完整；
2. 再到 **服务 → OpenClash → 插件设置 → 调试日志 → 生成** 获取调试日志；
3. 检查订阅实际策略组是否包含配置的 `PROXY_POLICY_GROUP`，例如 `🚀 手动选择`。

## Bot 双 DNS 查询说明

Bot 的国内/国外 DNS 查询是独立的公网 JSON DNS 查询，仅用于比较不同解析视图、获取真实 IP 和调用 GeoIP API，不会替换 OpenClash 的 DNS 配置，也不会写入 Mihomo 运行时 DNS。

默认端点为：国内阿里云 `/resolve` 主、腾讯 DNSPod 备；国外 Cloudflare 主、Google 备。两组并行执行，组内按主备故障转移。OpenClash 使用 Fake-IP 时，Bot 会过滤 `198.18.0.0/15` 和对应 Fake-IP 地址，不把合成地址交给 GeoIP。

排障 OpenClash 本身时仍应先检查 **系统 → 软件包**，再从 **服务 → OpenClash → 插件设置 → 调试日志 → 生成** 获取完整日志；Bot 的公网 DNS 结果不能替代 OpenClash 调试日志。
