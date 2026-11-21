# iStoreOS MCP 权限管理
<!-- impeccable:product-schema 1 -->

## Platform
web

## Stack
生产使用 OpenWrt LuCI 原生视图、rpcd exec 插件和 ACL；Go MCP 服务复用现有 loopback listener，后台管理请求通过 mode-0600 Unix socket。x86_64 安装包由 Go 静态构建和 IPK 脚本生成。

## Users
路由器管理员，在电脑或手机上管理 Codex、OpenClaw 等客户端，审批 AI 对路由器的操作。

## Product Purpose
按客户端管理查询与操作授权，在 AI 操作前看清参数、影响及恢复方式。

## Operating Context
iStoreOS 22.03.7、x86_64、OpenClash。管理员通过局域网或 VPN 的 LuCI 登录管理；MCP 仍只绑定 loopback，由现有 SSH 隧道接入 Codex。LuCI 页面展示设备实时状态。

## Capabilities and Constraints
五个页面：概览、客户端、权限、审批、审计。查询可长期授权；普通操作默认15分钟；敏感逐笔申请5分钟有效；可回滚修改由用户在120秒内确认保留。全局停止保留查询并取消待执行操作。重启清除临时授权和待执行审批，恢复未确认修改。多客户端独立令牌。日志7天或16MB。仅约束MCP，不约束root SSH。

## Evidence on Hand
完整用户确认见 `.grill/istoreos-mcp-permission-console.md`。原型中的节点、时间、请求和令牌均为模拟内容。

## Product Principles
- 拒绝默认操作授权。
- 具体参数绑定逐笔审批。
- 恢复和用户确认不能由AI绕过。
- 明确区分演示状态与真实设备状态。

## Production implementation
`server/files/www/luci-static/resources/view/openwrt-mcp.js` and its CSS implement the five approved LuCI pages. `server/management.go` serves a root-only Unix socket API and independent per-client query/operation limits. `server/platform.go` implements the fixed operation allowlist, OpenClash loopback access, isolated UCI staging, bounded recovery snapshots and runtime checks. `server/files/etc/init.d/openwrt-mcp-recovery` supervises recovery independently from the MCP daemon.

The package is `server/openwrt-mcp_0.6.4_x86_64.ipk`. See `docs/production-install.md` for installation and operational boundaries. The interactive browser prototype remains under `prototype/` as a design reference; the LuCI page uses real device state.

