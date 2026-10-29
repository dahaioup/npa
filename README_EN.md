# NPA — Node Proxy Agent

English | [简体中文](README.md)

NPA is a single-binary, cross-platform management agent for intranet machines. It aggregates scattered Windows/Linux/macOS hosts into a manageable mesh via **reverse connections**, and provides remote execution, file distribution, tunnel mapping, desktop control, an MCP tool surface, and **guardian-mode automatic upgrades**.

- Single binary, zero external dependencies (CGO_ENABLED=0)
- Data plane only uses outbound connections from child nodes — NAT/firewall friendly
- Built-in MCP server, directly drivable by any AI program
- Versioning: `v0.4.1-<unix build timestamp>`

## Architecture Overview

```
Browser/AI ──► Web admin 127.0.0.1:56700
AI (MCP)  ──► Mesh entry :56701  ─┬─ /mcp (MCP Streamable HTTP)
                                  ├─ /api/file/* /api/exec (node file/exec plane)
                                  ├─ /api/topology /api/info (topology & discovery)
                                  └─ peer protocol (register/link/conn/open/exec/pty/relay_open)
Node ──outbound──► Upper :56701 (reverse WS: control + on-demand data channels)
Cross-domain ──► upper-of-upper proxy chain (peer_relay_fallback, per-hop require)
Upgrade ──► guardian process (backup/watchdog/restart/rollback/self-cleanup)
```

## Implemented Features ✅

### Node Access & Discovery
- ✅ **Reverse access**: children dial outbound to the upper node; control link plus on-demand data channels (exec/PTY/file/reverse TCP/relay)
- ✅ **mDNS discovery** (`_npa._tcp.local.`): advertises ID/name/version/peer port/uptime/reachable subnets; poll-based resolver with QU unicast answers, immune to Windows multicast egress/firewall issues
- ✅ **Upper auto-discovery**: with no seeds configured, probes off-subnet ranges via traceroute to find neighboring NPA nodes automatically (`peer_auto_discover_uppers`, on by default)
- ✅ **Cross-domain topology**: `/api/topology` with parent/missing, stale-while-revalidate cache + concurrent multi-level queries
- ✅ **Cross-domain access**: when direct dial fails, requests are proxied hop-by-hop through the holding upper node (`peer_relay_fallback`), with in-flight negotiation dedup + failure cooldown

### Remote Execution & Files
- ✅ **Remote execution**: `ssh.exec` (SSH nodes) and HTTP `/api/exec` on peer nodes (stdout/stderr truncated at 2 MiB, controllable timeout)
- ✅ **Node file plane**: `/api/file/{list,stat,read,download,upload,write,mkdir,remove,rename}`, fully streaming with no size limit (4 MiB in-memory cap for read/write), atomic temp-file rename, concurrent IO gate (≤2), idle-timeout watchdog
- ✅ **Cross-domain file distribution**: three-tier routing — direct HTTP → upper proxy chain → SFTP-over-WS fallback; transfers never occupy the control channel
- ✅ **SSH management**: encrypted credentials (AES-GCM), SFTP, interactive PTY (web terminal)
- ✅ **Tunnels & port mapping**: SSH tunnel restore, local port mapping (loopback-bound by default)

### Upgrade & Operations
- ✅ **Guardian-mode auto upgrade**: the new binary runs as a guardian via `--guardian-config` — backup old → stop old → replace → spawn → health-check every 5s → restart on failure → auto-rollback after 6 consecutive failures → self-cleanup on success; in-place same-name replacement throughout
- ✅ **Built-in upgrade runbook**: MCP tool `upgrade.guide` — any AI program connecting to a node can fetch the full upgrade procedure and pitfall list
- ✅ **Self-update push**: package push + one-time download token + pre-restart detection
- ✅ **Autostart**: Windows registry (PowerShell hidden-window launch, no console popup on login), macOS launchd, Linux systemd/XDG
- ✅ **Web admin UI**: nodes/SSH/tunnels/port mappings/topology/audit/settings pages, SSE event stream
- ✅ **Audit log**: MCP operation auditing with configurable retention
- ✅ **Crash protection**: recover middleware on the mesh entry — a single panicking request never takes down the process

### AI Integration
- ✅ **MCP server** (Streamable HTTP, `:56701/mcp`): `nodes.list`, `ssh.exec`, `file.*`, `tunnel.*`, `desktop.*`, `discovery.*`, `network.subnets`, `relay.*`, `upgrade.guide` and 20+ tools, each gated by capability flags
- ✅ AI programs can compose MCP tools into a fully automated "discover → deploy → upgrade → verify" loop

## Planned Features 📋

- 📋 **Domain mesh v2 (full)**: domain manager election, heartbeat liveness, and epoch/seq incremental sync core are landed (`internal/peer/domain.go`, unit-tested); remaining: active probing graph, real multi-process e2e tests
- 📋 **Data-plane robustness**: channel pool reuse after use, data-connection keepalive (NAT silent-drop protection)
- 📋 **Permission system redesign**: deliberately deferred for stability — the file/exec plane currently has no authentication; intranet use recommended until redesigned
- 📋 **Upgrade transactionality**: drain in-flight requests before restart + new-version acknowledgement loop
- 📋 **Resumable transfers**: chunked large-file transfer with SHA256 and Range resume
- 📋 **Topology missing consumption**: failed DialNode targets enter holder cooldown to avoid repeated negotiation
- 📋 **Delta package distribution**: upper-node caching/dedup when many children pull concurrently

> Design decision: **direct-connection negotiation between domains was explicitly dropped** (2026-10-26); all cross-domain access goes through proxy chains. Re-evaluation triggers: sustained high throughput or cross-WAN links.

## Quick Start

```bash
# Build (version stamp injected at build time)
scripts/build-release.sh          # 7-platform cross compile + zip

# Run (default data dir ~/.npa)
./npa                             # Web admin at http://127.0.0.1:56700
./npa --version                   # v0.4.1-<unix ts>
```

To join a node to the mesh: run NPA on the new machine, approve its registration on the upper node's web "Nodes" page, or rely on `peer_uppers`/mDNS auto-discovery.
