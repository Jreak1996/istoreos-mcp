# iStoreOS MCP 权限管理

## Platform

LuCI 管理界面与 OpenWrt / iStoreOS 上的 Go MCP 服务。

## Stack

生产界面位于 `files/www/luci-static/resources/view/openwrt-mcp.js`，样式位于相邻资源目录；rpcd 管理桥接通过 root-only Unix socket 调用服务，LuCI 权限由 `files/usr/share/rpcd/acl.d/` 配置。Go MCP 服务只在 loopback 上监听，IPK 由仓库根目录的 Makefile 与 `mkipk.sh` 构建。

## Users

通过电脑或手机登录 LuCI 管理路由器的管理员，以及使用 Codex、OpenClaw 等 MCP 客户端的操作者。

## Product purpose

按客户端控制路由器查询与操作权限。在每项敏感操作执行前，让管理员看到目标、参数、影响和恢复方式。

## Operating context

当前部署目标为 iStoreOS 22.03.7、x86_64、OpenClash。管理员通过 LAN 或 VPN 的 LuCI 登录；远端 MCP 客户端用 SSH 隧道连接路由器本地 MCP 端口。LuCI 概览读取真实设备状态，不依赖原型模拟数据。

## Capabilities and constraints

LuCI 包含概览、客户端、权限配置、待审批和审计五个视图。查询按 system、network、devices、logs 分组；发现 ubus 对象只返回对象/方法签名。新客户端默认关闭查询和写操作权限。管理员可以单独开放查询组、限制调用频率，并为普通节点/模式操作发放 1 至 60 分钟的临时授权。OpenClash 服务重启、允许范围内的配置写入和静态 DHCP 租约始终需要针对一笔请求逐笔批准。

审批申请 5 分钟后过期。可恢复的配置更改由独立恢复服务监视；管理员必须在 120 秒内从可工作的管理连接确认保留，否则恢复此前值。服务重启会清除临时授权并取消未执行的申请；未完成的恢复不能由 MCP 客户端取消。全局操作暂停会清除临时授权和待处理申请，但不撤销查询权限。

服务端只保存 bearer token 的摘要；令牌创建时显示一次。审计按操作摘要记录，不应保存原始令牌。此权限模型只约束 MCP；root SSH 仍是独立的管理员维护通道。

## Product principles

- 新客户端默认拒绝查询/操作访问，逐组授予最小权限。
- 审批绑定具体请求和预览摘要，执行前重新核对内容。
- 只有管理员能在 LuCI 批准、拒绝或确认回滚。
- 页面明确区分真实设备状态和静态原型。

## Implementation map

- MCP HTTP 服务与命令行入口：`server.go`、`main.go`、`tools.go`。
- OpenWrt 操作白名单、OpenClash controller、配置暂存和恢复：`platform.go`。
- LuCI 管理 RPC 与授权边界：`management.go`、`files/usr/libexec/rpcd/openwrt-mcp-admin`。
- 客户端、申请和持久化状态机：`control/`。
- LuCI 页面、菜单、样式与 ACL：`files/`。
- IPK 构建：根目录 `Makefile`、`mkipk.sh`。

交互原型仍在 `prototype/`，只用于设计讨论；不具备设备权限或真实执行能力。安装、客户端配置和具体权限范围见 `docs/`。
