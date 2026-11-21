# iStoreOS MCP permissions console

## Included in 0.6.4

- LuCI menu entry **服务 → MCP 权限管理**, protected by the existing LuCI administrator login and an rpcd ACL.
- Five views: overview, clients, permissions, approvals and audit. The page uses real router and MCP state.
- The overview reads live interface status/counters and active DHCP leases through LuCI's authenticated read ACL.
- A local rpcd bridge reaches the daemon through `/etc/openwrt-mcp/admin.sock` (socket mode `0600`, parent directory mode `0700`). The bridge exposes fixed management methods only.
- A separate procd recovery worker watches protected UCI changes. Configuration edits use a private UCI staging directory, a before-state check, a recovery journal, bounded service reloads and runtime checks. Rollback targets the approved keys and preserves unrelated edits.
- MCP stays bound to `127.0.0.1`; the web console does not add a TCP admin endpoint.

## Install on iStoreOS 22.03.7 x86_64

```sh
opkg install /tmp/openwrt-mcp_0.6.4_x86_64.ipk
```

The package enables and starts the recovery worker before restarting the MCP service. It preserves `/etc/config/openwrt-mcp` and `/etc/openwrt-mcp`, restarts rpcd to register the new ACL and executable plugin, and adds the LuCI view. Check **状态 → 系统日志** or run:

```sh
/etc/init.d/openwrt-mcp status
/etc/init.d/openwrt-mcp-recovery status
ubus call openwrt-mcp-admin status
```

The first LuCI view is under **服务 → MCP 权限管理**. Existing MCP tokens are imported with their prior read policies; those old method, scope, expiry and MFA restrictions remain in force until that query group is explicitly changed in LuCI. New clients have no query or operation permission. A newly created token is displayed once.

## Client operations

The MCP server adds `request_operation`, `execute_operation`, `operation_status` and `openclash_status`. Ordinary `node` and `mode` actions require a temporary grant. OpenClash restart, allowlisted UCI configuration fields and static DHCP leases always require exact per-request approval. Reversible configuration requests require a human confirmation from a working management connection within 120 seconds; otherwise the protected fields are restored. A daemon restart also clears temporary grants and cancels unexecuted requests.

The admin page accepts only the existing typed operation kinds. There is no generic shell, arbitrary UCI, `uci_apply` or confirmation endpoint exposed to an MCP bearer token. Root SSH remains a separate maintenance path outside the permission boundary.

## Rollback

The package upgrade keeps the prior policy file and token directory. Before a manual rollback, stop the recovery and MCP services, restore the saved 0.5.0 binary and init script, then start the MCP service. The package removal script refuses uninstall while a protected operation is waiting for confirmation or recovery. Keep recovery records under `/etc/openwrt-mcp/recovery` until they reach a terminal state.

## Build

`server/mkipk.sh` packages the static binary and LuCI assets. Build for the target with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, then pass the binary, version and `x86_64` to the script. The checked-in IPK is the artifact prepared for this device.

