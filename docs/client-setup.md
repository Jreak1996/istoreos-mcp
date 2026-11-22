# Codex 与 OpenClaw 客户端接入

## 前置条件

1. 路由器已安装并运行 MCP 服务。
2. 在 LuCI 的 **服务 → MCP 权限管理 → 客户端** 为每个客户端创建独立身份。令牌只显示一次，复制并妥善保存。
3. 在运行客户端的电脑/环境中建立 SSH 本地转发并保持运行：

```sh
ssh -N -L 127.0.0.1:8730:127.0.0.1:8730 istore
```

如果没有 `istore` 别名，改成 SSH 用户和路由器 LAN 地址。以下配置使用本机隧道地址，不要求路由器开放 8730 端口。

```sh
curl http://127.0.0.1:8730/health
```

返回 `openwrt-mcp <version> ok` 表示本地隧道到达了服务。MCP URL 是 `http://127.0.0.1:8730/mcp`。

## Codex

Codex CLI 和桌面客户端使用 Codex 配置中的 MCP server 项。推荐不要把 token 写入 TOML；使用环境变量名由 Codex 在启动时读取。

在 Windows 设置用户级环境变量 `ISTOREOS_MCP_TOKEN`，或通过你的操作系统凭据/启动环境管理方式仅向 Codex 进程提供此变量。修改后完全退出并重新启动 Codex，使其继承新环境。不要将真实令牌粘贴进代码仓库、issue、聊天或共享配置文件。

在 `%USERPROFILE%\.codex\config.toml`（Windows）或 `~/.codex/config.toml`（Linux/macOS）添加：

```toml
[mcp_servers.istoreos]
url = "http://127.0.0.1:8730/mcp"
bearer_token_env_var = "ISTOREOS_MCP_TOKEN"
enabled = true
enabled_tools = [
  "ubus_list",
  "ubus_call",
  "logread",
  "request_operation",
  "execute_operation",
  "operation_status",
  "openclash_status",
]
```

工具 allowlist 只控制 Codex 展示哪些服务工具；路由器端 LuCI 权限仍会再次检查每次调用。若你不需要代理节点/模式切换，把 `execute_operation` 从列表中去掉；敏感操作仍要通过 `request_operation` 申请并由管理员在 LuCI 逐笔审批。

重启 Codex 后，在 MCP 状态/工具列表中确认 `istoreos` 已连接。Codex 配置字段会随版本演进，字段说明与界面入口请参阅[官方 Codex 配置参考](https://developers.openai.com/codex/config-reference/)和[MCP 文档](https://developers.openai.com/codex/mcp/)。

## OpenClaw

在负责运行 agent 的 OpenClaw Gateway 的配置中加入 server 定义。以 JSON5 为例：

```json5
{
  mcp: {
    servers: {
      istoreos: {
        url: "http://127.0.0.1:8730/mcp",
        transport: "streamable-http",
        headers: {
          Authorization: "Bearer ${ISTOREOS_MCP_TOKEN}",
        },
        toolFilter: {
          include: [
            "ubus_list",
            "ubus_call",
            "logread",
            "request_operation",
            "execute_operation",
            "operation_status",
            "openclash_status",
          ],
        },
      },
    },
  },
}
```

在 Gateway 进程的运行环境中设置 `ISTOREOS_MCP_TOKEN`，保存配置后使用 OpenClaw MCP 控制界面或 CLI 检查状态，并进行在线探测：

```sh
openclaw mcp status --verbose
openclaw mcp doctor istoreos --probe
```

若你的版本不接受按名称指定的 doctor 参数，可用 `openclaw mcp probe istoreos` 检查实际连接和工具列表。OpenClaw 配置应使用 `transport: "streamable-http"`，而不是把它当作 stdio server。Gateway 如运行在容器或远端 executor 内，SSH 隧道也必须从它实际能访问的网络命名空间提供 loopback 端口。

OpenClaw MCP 字段和 CLI 可能随版本调整；查看官方[连接 MCP server](https://docs.openclaw.ai/tools/mcp)、[传输与认证](https://docs.openclaw.ai/cli/mcp/transports)文档。

## 权限与工具过滤器如何配合

推荐的客户端工具列表不等于授予路由器权限。访问结果由三层共同决定：

1. Codex / OpenClaw 的工具过滤器是否展示该工具。
2. MCP 客户端令牌对应的 LuCI 查询组/普通操作 grant 是否允许该请求。
3. 对敏感操作，管理员是否在有效申请窗口内批准了完全相同的请求预览。

新客户端可以先只接入查询工具。确认系统、网络和设备信息正常后，再按需开放 OpenClash 状态或申请工具。只有确实需要自动执行已批准的节点/模式变更时才保留 `execute_operation`。
