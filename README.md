# agy-acp-muzzle

Headless agy runs full auto: every tool is approved and every shell command is
allowed. `agy-acp-muzzle` wraps Google's official Antigravity ACP kernel with:

- per-session lockdown;
- a Landlock sandbox;
- visible denials;
- history that survives restarts.

> **Status: pre-alpha.** The transparent relay works. Lockdown and sandboxing
> are designed (see [docs/DESIGN.md](docs/DESIGN.md)) but not implemented yet.

## How it works

```
ACP client (Paseo, Zed, ...)  <-- ACP -->  agy-acp-muzzle  <-- ACP -->  agy_acp_server (unmodified)
```

The muzzle starts the official `agy_acp_server` as a child process and relays
ACP (JSON-RPC over NDJSON) between it and the client, applying policy on the
way:

- **locked** sessions get no tools, no auto-approval, an empty workspace, no
  MCP servers and a Landlock sandbox;
- **open** sessions keep everything.

The client chooses the mode per session.

The kernel is never modified or redistributed. Install and authenticate it
yourself under the [Antigravity terms](https://antigravity.google/terms).

## Usage

```bash
go build -o agy-acp-muzzle ./cmd/agy-acp-muzzle
AGY_ACP_MUZZLE_KERNEL=/opt/agy-acp-server/agy_acp_server.par ./agy-acp-muzzle
```

As a Paseo custom ACP provider (`$PASEO_HOME/config.json`):

```json
{
  "agents": {
    "providers": {
      "agy": {
        "extends": "acp",
        "label": "Antigravity (muzzled)",
        "command": ["/usr/local/bin/agy-acp-muzzle"],
        "env": { "AGY_ACP_MUZZLE_KERNEL": "/opt/agy-acp-server/agy_acp_server.par" }
      }
    }
  }
}
```

## Development

```bash
go vet ./... && go test ./...
```

## License

Apache-2.0. Community project, not affiliated with Google or Paseo.
