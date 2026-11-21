# iStoreOS MCP

A Go-based MCP server for OpenWrt and iStoreOS with a native LuCI console for client permissions, time-limited grants, sensitive-operation approvals, audit history, and recovery.

This project is based on [GlassOnTin/openwrt-mcp](https://github.com/GlassOnTin/openwrt-mcp) and adds the iStoreOS LuCI permission console and router-specific packaging.

## Repository layout

- Go MCP server, OpenWrt platform integration, and tests at the repository root
- LuCI and rpcd integration under `files/`
- Browser prototype under `prototype/`
- Production install and rollback notes in [docs/production-install.md](docs/production-install.md)
- Product and interaction design in [PRODUCT.md](PRODUCT.md) and [DESIGN.md](DESIGN.md)

## Build

Use the Go version declared in `go.mod` and the shell tools listed by `mkipk.sh`. Build the server with `go build ./...`; create the OpenWrt package with `./mkipk.sh`. Review the install and rollback notes before installing on a router.

## Security model

The MCP listener is restricted to loopback. MCP clients receive separate bearer tokens and explicit query/action permissions. Sensitive actions require a LuCI approval bound to the requested operation. The server does not expose a general shell or arbitrary UCI write tool. Keep the MCP endpoint private and use an SSH tunnel when connecting a remote client.

## License

See [LICENSE](LICENSE).
