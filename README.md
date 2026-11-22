# iStoreOS MCP

把 OpenWrt / iStoreOS 的只读查询与受控路由操作接入 MCP 客户端，并通过 LuCI 让管理员管理权限、逐笔审批敏感操作和查看审计记录。

仓库基于 [GlassOnTin/openwrt-mcp](https://github.com/GlassOnTin/openwrt-mcp)，增加了 iStoreOS LuCI 权限管理界面、后台管理桥接和路由器 IPK 打包配置。

## 适用环境

当前部署目标是 **iStoreOS 22.03.7、x86_64、opkg**。MCP 服务只监听路由器本机回环地址 `127.0.0.1:8730)，远端 Codex 或 OpenClaw 通过 SSH 本地端口转发访问。不要把 8730 直接开放到局域网或公网。

其他 OpenWrt 版本、硬件架构以及基于 apk 的软件包环境尚未由本项目文档确认。安装前请先核对 [兼容性和安装步骤](docs/production-install.md)。

## 能力

- 读取系统信息、网络接口、在线设备、无线信息和过滤后的系统日志。
- 查询 OpenClash 当前代理模式、可选策略组与节点。
- 在 LuCI 中管理独立客户端令牌、查询权限组、调用频率和普通操作的限时授权。
- 逐笔审批 OpenClash 重启、允许范围内的 UCI 配置修改与静态 DHCP 租约操作。
- 对可回滚的配置修改设置恢复窗口；管理员需要从新的管理连接确认后才会保留。
- 查看操作申请状态与管理审计。MCP 客户端自身不能审批、确认或拓宽权限。

## 快速安装

仓库不保存可直接下载的 IPK。使用 Linux / WSL 环境按 [生产安装文档](docs/production-install.md) 构建并传到路由器：

```sh
make ipk ARCH=amd64 IPK_ARCH=x86_64
scp -O openwrt-mcp_0.6.4_x86_64.ipk istore:/tmp/openwrt-mcp.ipk
ssh istore 'opkg install --force-reinstall /tmp/openwrt-mcp.ipk'
```

安装后在 LuCI 的 **服务 → MCP 权限管理** 中创建客户端。新令牌只显示一次；创建弹窗会显示令牌并提供复制按钮。详细配置见：

- [生产安装、升级和回滚](docs/production-install.md)
- [权限模型和操作范围](docs/permissions-and-operations.md)
- [Codex 与 OpenClaw 接入](docs/client-setup.md)
- [故障排查](docs/troubleshooting.md)
- [开发和打包](docs/development.md)
- [产品说明](PRODUCT.md) 与 [界面设计记录](DESIGN.md)

## 安全边界

新客户端默认没有查询或操作授权。服务不会向 MCP 客户端开放通用 root shell 或任意 UCI 写入。普通 OpenClash 节点/模式切换须先取得短时授权；服务重启、配置更改和 DHCP 静态租约必须通过 LuCI 对具体申请逐笔批准。监听地址、token 生命周期、恢复与边界细节见权限文档。

这些限制只约束 MCP 通道；拥有 root SSH 权限的人仍可直接管理路由器。令牌存储、配置、审计和恢复状态位于路由器本地，应按路由器敏感数据管理。

## 许可证

见 [LICENSE](LICENSE)。
