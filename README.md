# agy-acp-muzzle

Headless agy runs full auto: every tool is approved and every shell command is
allowed. `agy-acp-muzzle` wraps Google's official Antigravity ACP kernel with:

- per-session lockdown;
- a Landlock sandbox;
- visible denials;
- history that survives restarts.

> **Status: pre-alpha.** The relay, the locked-mode policy and the Landlock
> sandbox are implemented and tested against a fake kernel; runtime tests
> against the real kernel are next (see [docs/DESIGN.md](docs/DESIGN.md)).

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

# locked session for untrusted input (or set AGY_LOCKDOWN=1)
./agy-acp-muzzle --locked
```

Locked mode needs the kernel to be logged in once (its credentials live in
`$GEMINI_HOME/antigravity-acp/acp_token.json`); each locked connection gets a
private temporary Gemini home holding only that file. The kernel then runs under
Landlock: it may execute only its own directory, write only that private root,
read a minimal set of system paths and connect only to TCP 443. Tune with
`--sandbox-exec`, `--sandbox-ro`, `--sandbox-rw` and `--sandbox-port`; locked
mode refuses to start where Landlock (ABI 4+) is unavailable unless
`--no-sandbox` is given.

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

`cmd/acp-smoke` is a scripted ACP client for manual runs against the real
kernel:

```bash
go build -o acp-smoke ./cmd/acp-smoke
./acp-smoke -prompt "Reply with PONG" -- ./agy-acp-muzzle --locked
```

## License

Apache-2.0. Community project, not affiliated with Google or Paseo.
