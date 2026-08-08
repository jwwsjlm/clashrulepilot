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

Bot 生成的个人 YAML 是 Mihomo `classical` rule-provider 内容；策略目标不写进 YAML，而是在 OpenClash 的 `rules:` 中通过 `RULE-SET` 指定：

```yaml
rules:
  - RULE-SET,my_proxy,🚀 手动选择
  - RULE-SET,my_direct,DIRECT
```

个人规则必须排在上游规则之前，否则上游的 `DOMAIN-SUFFIX` 规则可能先命中。

ClashRulePilot 生成的 `personal-overwrite.ini` 使用 `[YAML]` 和 `+rules` 前置插入。根据权威指南第 8 章，OpenClash 会先下载远程覆写到 `/etc/openclash/overwrite/`，再将覆写内容合并到运行配置。Bot 的本地 Aethersailor 索引只用于查询，不参与路由器防火墙链，也不会改变 nftables/iptables 透明代理规则。
