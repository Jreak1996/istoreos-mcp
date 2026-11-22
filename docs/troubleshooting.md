# 故障排查

排查时不要把完整 bearer token、OpenClash dashboard secret、订阅 URL、MFA seed 或未脱敏系统日志发给他人。

## LuCI 页面中令牌框是空白

修复版页面把令牌作为 textarea 的文本内容渲染，并在创建弹窗中提供“一键复制令牌”。若页面仍是旧代码：

1. 强制刷新浏览器（Windows/Linux: `Ctrl+F5`；macOS: `Cmd+Shift+R`）。
2. 在 LuCI 客户端页重新创建一个专用客户端并立即复制。令牌只显示一次，旧令牌无法从服务端取回。
3. 如果曾关闭弹窗或确定保存后才发现没复制，撤销该客户端并创建新 token。

剪贴板 API 受浏览器安全上下文和权限限制。若复制失败，弹窗会提示；选中只读文本框内容手动复制即可。

## MCP 返回连接失败

- 确认隧道窗口仍在运行：`ssh -N -L 127.0.0.1:8730:127.0.0.1:8730 istore`。
- 从 MCP 客户端所在的同一主机/网络命名空间打开 `http://127.0.0.1:8730/health`。
- 在路由器检查 `/etc/init.d/openwrt-mcp status` 与 `logread -e openwrt-mcp`。
- 若出现 connection refused，检查路由器服务是否运行、端口配置是否还是 loopback:8730、SSH 转发目标是否写成路由器上的 `127.0.0.1`。
- 若使用容器 OpenClaw，容器自己的 `127.0.0.1` 不会自动等于宿主机的 loopback；让隧道和 Gateway 位于同一网络环境。

不要为了排障把服务改绑到 `0.0.0.0` 或将路由器 8730 暴露到公网。

## HTTP 401 / 未授权

401 表示 token 缺失、错误或已撤销。检查 MCP server 是否使用当前客户端的完整 token、HTTP Authorization Bearer 是否由环境变量提供、客户端名称是否对应正确；再在 LuCI 撤销并重新创建。不要把 token 写入命令行参数或共享日志。

服务端只存 token 摘要，因此无法找回旧 token。不要为排障重新开放通用 `exec`。

## 工具没有出现

- 重启 Codex / OpenClaw Gateway，检查 MCP 配置 URL、transport 和 tool allowlist。
- Codex 使用 Streamable HTTP MCP server；OpenClaw 配置里显式设 `transport: "streamable-http"`。
- OpenClaw 运行 `openclaw mcp status --verbose` 及 `openclaw mcp doctor istoreos --probe`；Codex 查看 MCP server 状态面板或运行 `/mcp`。
- 检查是否在错误的本地/容器网络命名空间开启了 SSH 隧道。

## 查询被拒绝

- LuCI 中确认客户端未撤销，且相应查询组为“长期开放”或合法的旧策略继承。
- `ubus_list` 只列对象和签名；它不授予其他 ubus 方法权限。
- 旧策略继承时还要满足原来的工具、scope、到期和 MFA 要求。遗留策略未覆盖某个对象方法时，请先明确决定是否在 LuCI 开放对应查询组。
- `openclash_status` 要求显式 `network` 查询组；旧策略继承不能单独替代这项显式配置。
- 调用频率超过客户端每分钟上限时会被拒绝；可在 LuCI 查看并调整 1–600 次/分钟的上限。

## 操作申请或批准失败

- 节点切换只能选择真实存在的 selector 策略组与节点；mode 仅支持 `rule`、`global`、`direct`。
- 普通节点/模式执行需要 LuCI 中仍有效的限时授权（界面默认 15 分钟）。
- `request_operation` 只创建 5 分钟有效的申请，不会执行；由 LuCI 管理员逐笔审批。
- 批准时预览摘要已变化或申请过期时，服务会取消/拒绝原申请；重新读取状态后提交新申请。
- OpenClash 重启、受限配置项和静态租约都是敏感申请，不能通过普通 grant 直接执行。

## 配置修改后 LuCI 连接中断

管理地址或 uhttpd listen 变更后，旧浏览器会话可能断开。不要在旧会话上判断变更失败；从新的管理地址/连接重新登录，在 120 秒恢复窗口内打开 MCP 权限页并确认保留。如果不能恢复连接或没有确认，恢复服务应回滚变更。

检查恢复服务：`/etc/init.d/openwrt-mcp-recovery status`。不要手工删除未结束的恢复记录。如果状态仍为 waiting，等待独立恢复服务完成，必要时查看 `logread -e openwrt-mcp` 和 MCP 恢复日志。

## 升级/卸载被拒绝

当受保护的配置更改等待用户确认或恢复时，包卸载会拒绝继续。这是保护机制。先在 LuCI 查看待处理操作，完成确认或等待回滚后再重试。不要用强制删除绕过恢复记录。
