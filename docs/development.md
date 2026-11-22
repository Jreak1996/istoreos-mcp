# 开发、构建与打包

## 目录

```text
.
├── main.go / server.go / tools.go       Go 命令入口、MCP HTTP 与工具
├── management.go / platform.go         LuCI 管理桥接与 OpenWrt 操作实现
├── control/                             客户端权限、申请状态机和恢复协调
├── files/                               init 脚本、rpcd ACL、菜单、LuCI 页面和 CSS
├── prototype/                           仅用于讨论的静态交互原型
├── ui/                                  旧 GL.iNet OUI 界面资源
├── Makefile                             build、test、ipk 与可选 install 目标
└── mkipk.sh                             不依赖 OpenWrt SDK 的 opkg 包装脚本
```

Go 源码位于仓库根目录；包中页面来自 `files/`。请不要照旧资料从不存在的 `server/` 子目录运行构建。

## 工具链与构建

`go.mod` 指定 Go 1.26.1。构建 x86_64 iStoreOS opkg 包：

```sh
make ipk ARCH=amd64 IPK_ARCH=x86_64
```

Makefile 的 `build` 依赖 `test`，会先执行 `go vet ./...` 与 `go test ./...`，再使用 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64` 静态构建。随后 `mkipk.sh` 把二进制、procd init/recovery 脚本、默认 UCI、rpcd ACL、LuCI 菜单和静态资源打成 IPK。

开发机推荐 Linux 或 WSL。Go 模块版本应与 go.mod 相容；打包脚本依赖 POSIX shell、tar/gzip 和常用 coreutils。若只是构建而不安装，不需要 OpenWrt SDK。输出文件名形如 `openwrt-mcp_<version>_x86_64.ipk`。

## 可选 Make 目标

- `make test`：vet 和 Go 测试。
- `make build ARCH=amd64`：测试后构建 Linux amd64 可执行文件。
- `make ipk ARCH=amd64 IPK_ARCH=x86_64`：构建路由器安装包。
- `make install ROUTER=istore ARCH=amd64`：经 SSH 直接部署二进制/脚本；该目标不会像 IPK 安装那样提供包升级留存记录。
- `make install-ipk ROUTER=istore ARCH=amd64 IPK_ARCH=x86_64`：构建并经 SSH 安装 IPK。确认 alias、密钥和目标设备后再执行。

## 发布前人工核对

- `opkg print-architecture` 与包的 `IPK_ARCH` 一致。
- `mkipk.sh` 包含 MCP binary、MCP 与恢复 init 脚本、默认 UCI、rpcd bridge/ACL、LuCI 菜单/view/CSS。
- 升级不覆盖 live token、权限与状态目录；卸载脚本在未完成恢复时拒绝继续。
- 目标设备上的 listener 仍为 `127.0.0.1:8730)，LuCI ACL 与新菜单可用。
- 逐笔审批应绑定未变化的预览摘要；恢复服务可在主服务停止后运行。
- 文档不应将 `prototype/` 标成生产界面，也不应承诺仓库里有未提交的 IPK。

## 手动安装注意

若仅将网页脚本热修复到设备，先备份原文件、替换对应静态资源并确认文件权限。IPK 后续重装会再次从仓库 `files/` 覆盖该资源。不要复制路由器的 `/etc/openwrt-mcp`、真实 token 或 dashboard secret 到源码仓库。
