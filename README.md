# agy-acp-muzzle

Headless agy runs full auto: every tool is approved and every shell command is
allowed. `agy-acp-muzzle` wraps Google's official Antigravity ACP kernel and
gives each session exactly the access you choose, with the kernel's own
resource and reliability gaps fixed on the way.

> **Status: alpha.** Tested against the official kernel (`agy_acp_server`
> 1.4.5) directly and as a Paseo provider; see [docs/DESIGN.md](docs/DESIGN.md)
> for the design and measured results.

## Features

- **Per-session lockdown.** A locked session gets:
  - no built-in tools, no MCP servers and no client filesystem or terminal;
  - an empty private workspace and Gemini home;
  - every permission request rejected inside the muzzle, so client-side
    auto-accept never sees one.

  An open session keeps everything. The client picks the mode per session.
- **Landlock sandbox** for locked kernels: execute only the kernel itself,
  write only the private directory, read a minimal set of system paths,
  connect only to TCP 443 (plus the kernel's own loopback IPC).
- **Visible failures:**
  - denied actions, kernel errors reported as text, and empty turns become
    JSON-RPC errors instead of quiet successes;
  - a watchdog cancels turns whose kernel goes silent;
  - a turn cut off by a crash of the kernel's harness is continued
    automatically (twice by default) before it fails;
  - a kernel crash fails the request in flight, and the next request recovers.
- **History that survives restarts:** `session/load` replays the
  conversation from the kernel's own store.
- **Cheap when idle:** an idle kernel is stopped and transparently restored
  on the next request (about 4 MiB per idle agent instead of about 450 MiB).
  Client probes for modes and models are answered from a cache without
  starting a kernel.
- **Clean workspaces:** `run_workflow` (which writes a skill file into the
  workspace) is disabled by default; any built-in can be disabled.
- **Clean process tree:** the kernel and its per-session harnesses are
  stopped together, and orphans are reaped.

## How it works

```
ACP client (Paseo, Zed, ...)  <-- ACP -->  agy-acp-muzzle  <-- ACP -->  agy_acp_server (unmodified)
```

The muzzle starts the official kernel as a child process and relays ACP
(JSON-RPC over NDJSON) between it and the client, applying policy on the way.
The kernel is never modified or redistributed: you install and authenticate it
yourself, under the [Antigravity terms](https://antigravity.google/terms).

## Install

1. **The muzzle:** download a static binary from
   [Releases](https://github.com/Arsenikki/agy-acp-muzzle/releases) and
   verify it against `SHA256SUMS`, or build it yourself:

   ```bash
   go build -o agy-acp-muzzle ./cmd/agy-acp-muzzle
   ```

2. **The kernel:** Google publishes it as `antigravity-acp` in the ACP agent
   registry. Unpack the archive for your platform somewhere local and fast
   (a kernel start reads several hundred MiB, so avoid slow network storage).

3. **Log in once** with the kernel's consumer OAuth login (`oauth-personal`):
   `agy-acp-muzzle --login` prints the authorization link; open it anywhere,
   sign in, and paste back the `http://localhost:...` address the browser ends
   up on (it does not load; that is expected on a headless machine).
   Its credentials end up in `$GEMINI_HOME/antigravity-acp/acp_token.json`
   (`$GEMINI_HOME` defaults to `~/.gemini`). Clients that never call ACP
   `authenticate` (such as Paseo) also need the method in
   `$GEMINI_HOME/antigravity-acp/settings.json`:

   ```json
   {"auth": {"type": "oauth-personal"}}
   ```

## Usage

```bash
export AGY_ACP_MUZZLE_KERNEL=/opt/agy-acp-server/agy_acp_server.par
agy-acp-muzzle             # open session
agy-acp-muzzle --locked    # locked session (or AGY_LOCKDOWN=1)
```

As a Paseo custom ACP provider (`$PASEO_HOME/config.json`):

```json
{
  "agents": {
    "providers": {
      "agy-muzzle": {
        "extends": "acp",
        "label": "Antigravity (muzzled)",
        "command": ["/usr/local/bin/agy-acp-muzzle"],
        "env": { "AGY_ACP_MUZZLE_KERNEL": "/opt/agy-acp-server/agy_acp_server.par" }
      }
    }
  }
}
```

A single run can then be locked with `paseo run --env AGY_LOCKDOWN=1 …`.

## Configuration

| Flag | Environment | Default | Purpose |
|---|---|---|---|
| `--kernel` | `AGY_ACP_MUZZLE_KERNEL` | | kernel `.par` or binary |
| `--locked` | `AGY_LOCKDOWN` | off | locked session |
| `--idle-timeout` | `AGY_ACP_MUZZLE_IDLE_TIMEOUT` | `3m` | stop an idle kernel (`0` keeps it) |
| `--turn-timeout` | `AGY_ACP_MUZZLE_TURN_TIMEOUT` | `10m` | fail a turn after this long without kernel output |
| `--turn-guard` | | on | fail kernel-error and empty turns |
| `--auto-continue` | `AGY_ACP_MUZZLE_AUTO_CONTINUE` | `2` | continue a turn the kernel's harness crashed in up to this many times (`0` disables) |
| `--disabled-tools` | `AGY_ACP_MUZZLE_DISABLED_TOOLS` | `run_workflow` | built-ins removed from open sessions |
| `--flat-models` | | off | pass the kernel's flat per-thinking-level model list (default: one model per family plus a thinking-level option, like Claude in Paseo) |
| `--no-cache` | | off | always start the kernel for probes |
| `--sandbox-exec`, `--sandbox-ro`, `--sandbox-rw`, `--sandbox-port` | | | extend the locked Landlock rules |
| `--no-sandbox` | | off | locked mode without Landlock (debugging only) |
| `--login` | | | log the kernel in (consumer OAuth) on a headless machine, then exit |
| `--log-file`, `-v` | `AGY_ACP_MUZZLE_LOG_FILE`, `AGY_ACP_MUZZLE_DEBUG` | stderr, info | logging (many clients discard stderr) |

Locked mode requires Landlock ABI 4 or newer (Linux 6.7+) and refuses to
start without it unless `--no-sandbox` is given.

## Limitations

- **Memory while active:** a running turn costs about 450 MiB (about 295 MiB
  of it the kernel's Python runtime), more than the agy CLI. Idle shutdown
  removes that cost between turns.
- **Models:** the official ACP kernel lists only Gemini models for clients
  other than a few IDEs. The muzzle doesn't change that.
- **Platform:** Linux only for the sandbox and process management.

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
