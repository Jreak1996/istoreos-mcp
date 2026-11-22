# iStoreOS 安装、升级与回滚

本文面向 iStoreOS 22.03.7、x86_64、opkg 设备。当前示例与用户实际设备配置一致。其他发行版、CPU 架构或 apk 软件包环境不应直接照搬。

## 1. 兼容性检查

先在路由器 SSH 终端确认：

```sh
uname -m
opkg --version
opkg print-architecture
command -v ubus
command -v uci
command -v procd
df -h /overlay
free -h
```

本项目目标是 `x86_64` 和 `opkg`；需有 `ubus`、`uci`、`procd`、rpcd 与 LuCI。当前 iStoreOS 22.03.7 满足此包的目标环境。安装前检查 overlay 剩余空间，升级包还需要临时空间。

MCP 默认仅监听 `127.0.0.1:8730`，这是有意的安全边界。管理电脑需能 SSH 到路由器，或运行在同一路由器上。不要把监听地址改成 `0.0.0.0`，也不要将 8730 做公网端口映射。

## 2. 构建 x86_64 IPK

仓库根目录就是 Go 服务源码目录，**没有 `server/` 子目录**。在 Linux 或 WSL 中准备 Go 1.26.1（版本见 `go.mod`）、GNU Make 和项目打包脚本需要的 shell/tar 工具，然后从仓库根目录执行：

```sh
go version
make ipk ARCH=amd64 IPK_ARCH=x86_64
ls -lh openwrt-mcp_0.6.4_x86_64.ipk
```

`make ipk` 会先经过 Makefile 的 `build` 依赖；该目标会执行 `go vet ./...`、`go test ./...`，再以 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 编译并调用根目录的 `mkipk.sh`。IPK 名称中的版本从 `main.go` 读取。仓库不提交 IPK 二进制，因此每次需要从源码构建。

## 3. 上传并安装

如果 SSH 主机别名 `istore` 已在当前 Linux/WSL 环境的 `~/.ssh/config` 配好：

```sh
scp -O openwrt-mcp_0.6.4_x86_64.ipk istore:/tmp/openwrt-mcp.ipk
ssh istore 'opkg install --force-reinstall /tmp/openwrt-mcp.ipk && rm -f /tmp/openwrt-mcp.ipk'
```

`scp -O` 使用旧版 SCP 协议，可兼容没有 SFTP 子系统的 OpenWrt Dropbear。若当前设备名或地址不同，请替换 `istore`。在 Windows PowerShell 下也可使用系统 OpenSSH 的 `scp -O` 和 `ssh`，但须先确认 SSH 主机别名、密钥与构建文件路径在该终端可用。

升级脚本保留现有的 `/etc/config/openwrt-mcp` 与 `/etc/openwrt-mcp` 状态目录，不会把已有令牌明文重新显示。包会启用并启动独立恢复服务、注册 rpcd ACL/插件并重启相关服务。即使同一版本号重新安装，也使用 `--force-reinstall`。

## 4. 检查服务和 LuCI 页面

在路由器上执行：

```sh
/etc/init.d/openwrt-mcp status
/etc/init.d/openwrt-mcp-recovery status
ubus call openwrt-mcp-admin status '{}'
wget -qO- http://127.0.0.1:8730/health
```

预期：MCP 与恢复服务为 running；ubus 返回管理状态；health 输出包含 `openwrt-mcp <version> ok`。登录 LuCI 后打开 **服务 → MCP 权限管理**（路由为 `/cgi-bin/luci/admin/services/openwrt-mcp`）。页面需要 LuCI 管理员登录与项目 rpcd ACL。

在客户端页创建 Codex 或 OpenClaw 客户端。令牌只在创建成功时返回一次；先复制并保存到客户端的凭据管理方式中，再点击“已安全保存”。服务端只保存 token 摘要，之后无法恢复原令牌。若弹窗关闭或令牌遗失，撤销该客户端并创建新令牌。

## 5. 建立 SSH 隧道并接入 MCP

在运行 Codex / OpenClaw 的电脑或同一网络命名空间内打开一个终端并保持隧道：

```sh
ssh -N -L 127.0.0.1:8730:127.0.0.1:8730 istore
```

本地 MCP 地址为 `http://127.0.0.1:8730/mcp`，健康检查为 `http://127.0.0.1:8730/health`。关闭 SSH 会话即关闭隧道。Codex 与 OpenClaw 的配置分别见 [客户端接入文档](client-setup.md)。

若 OpenClaw Gateway 运行在容器或远端执行环境里，`127.0.0.1` 指向 Gateway 所在环境。隧道必须在同一网络命名空间内运行，并继续限制本地监听；不要为了连接容器而把路由器 MCP 端口开放到外部网络。

## 6. 升级前与回滚

常规包升级会保留 UCI 配置与 `/etc/openwrt-mcp` 下的 token、权限、审计和恢复状态。对于非标准手工修改，升级前使用 root-only 备份保存配置和状态；其中包含令牌摘要、MFA 等敏感信息，应保存在受保护的位置，切勿上传到 GitHub 或发到聊天。

若只需回滚本次 LuCI 静态页面热修复，可在路由器恢复先前备份（实际文件名以 SSH 命令输出为准）：

```sh
cp -p /root/openwrt-mcp.js.bak.<时间戳> /www/luci-static/resources/view/openwrt-mcp.js
chmod 0644 /www/luci-static/resources/view/openwrt-mcp.js
```

之后在浏览器强制刷新。完整 IPK 回滚应使用从可信来源保留的上一版包，并在安装前确认它与当前 opkg 架构匹配。不要手工删除恢复日志；独立恢复服务需要它来完成尚未结束的操作。移除包时，若存在等待确认或回滚的受保护变更，卸载脚本会拒绝删除。

## 7. 卸载

确认没有待确认的可恢复操作后，才执行：

```sh
opkg remove openwrt-mcp
```

卸载不会替管理员撤销独立管理任务；先在 LuCI 结束等待中的审批或恢复流程。升级、卸载与配置回滚期间不要删除 `/etc/openwrt-mcp/recovery` 中尚未结束的记录。
