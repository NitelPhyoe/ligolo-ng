# AGENTS.md

Context for AI agents and contributors working on the Sliver integration in this
ligolo-ng fork. Everything here was established by reading Sliver source and by
building and testing the extension locally — not assumed.

## Goal

Ship the ligolo-ng **agent** as a **Sliver C2 extension**, mirroring the setup in
the chisel fork (`../chisel`). Only the agent is built; the proxy/relay is not
part of the extension.

## Layout

| File | Role |
| --- | --- |
| `cmd/agent/extension.go` | `//go:build cgo`; Sliver entrypoint ABI + task registry |
| `cmd/agent/main.go` | Agent logic; `run()`/`runContext()` are shared by CLI and extension |
| `extension.json` | Sliver extension manifest (Windows amd64/386 DLLs only) |
| `Makefile` | `windows_binaries`, `sliver`, `extension` targets |
| `README.md` | "Sliver C2 extension" section |

## Build and install

On a Linux build host (Kali is fine):

```sh
sudo apt install -y gcc mingw-w64
make sliver          # builds both DLLs, installs into ~/.sliver-client/extensions/ligolo
make extension       # install only
```

- `windows_binaries` uses `CGO_ENABLED=1`, `-buildmode=c-shared`, mingw cross
  compilers, and builds **only** `./cmd/agent`.
- Output goes to `./dist/` (gitignored).
- Override the install path with `SLIVER_EXT=/some/path`.
- **Restart the Sliver client after installing** — extensions load at startup.

## Usage

```sh
ligolo -- connect 10.10.14.173:11601 -ignore-cert
ligolo -- bind 0.0.0.0:11601
ligolo -- list
ligolo -- stop 1
```

`connect <addr>` / `bind <addr>` are translated to ligolo's native flags
(`-connect <addr>` / `-bind <addr>`) before the flag parser sees them. Everything
after the address is passed through unchanged, so any agent flag works
(`-ignore-cert`, `-accept-fingerprint`, `-retry`, `-proxy`, `-ua`, ...).

The `--` matters: Sliver's shell parses its own flags first and only passes
positional arguments through.

## How it works

### The ABI

Sliver calls a native extension export as:

```c
int32_t entrypoint(char *buffer, uint32_t size, int32_t (*callback)(char *, int32_t))
```

- `buffer`/`size`: raw argument string, not null-terminated, not `argv`-split.
  `extension.go` does `C.GoStringN` then `strings.Fields`.
- `callback`: **only valid during the call**. Report before returning.
- Return `0` success, non-zero failure.
- On `windows/386` the callback is `stdcall` (the `SLIVER_CALLBACK` macro in the
  cgo preamble). 64-bit Windows and Unix use the default convention.
- The manifest's `entrypoint` must match the exported symbol.

### Why the entrypoint does not block

Extensions are loaded **into the implant process** and invoked with
`CallExportWithArgs`; the RPC completes when the export returns. So the agent
runs in a goroutine and the entrypoint returns after a short wait (2s) that
surfaces immediate argument/connection errors through the callback.

Unlike chisel — whose `Start`/`StartContext` return immediately — ligolo's agent
loop runs until the connection is lost, so it genuinely needs the goroutine.

### Changes to `main.go`

The agent's logic is unchanged; it was refactored so it can be driven by both
the CLI and the extension:

- `main()` → `run(args)` → `runContext(ctx, args)`.
- Errors are **returned** instead of `logrus.Fatal` (`Fatal` calls `os.Exit`,
  which would kill the implant). `main()` still calls `Fatal` itself, so CLI
  behaviour is unchanged.
- A `FlagSet` is used instead of the global `flag.CommandLine`.
- `ctx` is threaded through `connect`, `bind`, `wsconnect`, and the retry sleeps
  (`sleepContext`) so `stop` interrupts promptly rather than waiting out a
  20s reconnect delay.
- `connect`/`wsconnect` close the yamux connection on cancellation, which
  unblocks `Accept`.

**Maintenance note:** keep the refactor thin. Prefer returning errors over
`Fatal` in any code the extension can reach, and keep `ctx` flowing through new
blocking calls, or `stop` will hang.

### Task registry

`sliverTask{id, command, cancel, errCh}` in a mutex-guarded map.

- `list` — `taskID=N command=ligolo -connect ...`
- `stop <id>` — `cancel()`; the connection closes and the goroutine exits.
- A task is only registered if it is still alive after the 2s window; one that
  fails immediately is reported as an error and never listed.

### Why not an alias

Same two blockers as documented in the chisel fork:

1. Donut only resolves the `entrypoint` export for DLLs (`donut.c:1542`), so a
   `.dll` without it fails with `DONUT_ERROR_DLL_FUNCTION` (error code 12).
2. Aliases always sideload with `Kill: true`, and the implant waits for the
   entrypoint thread then kills the host process — incompatible with a
   long-running agent.

## Verified behaviour

Tested locally via a `dlopen` harness against a macOS `c-shared` build:

| Call | Result |
| --- | --- |
| *(empty)* | usage error, rc=1 |
| `bogus` | unknown command, rc=1 |
| `connect` | "needs an address", rc=1 |
| `connect 127.0.0.1:1` | "initial connection failed", rc=1 (not listed) |
| `connect 127.0.0.1:19555 -ignore-cert` | `task 1 started`, rc=0 |
| `list` | shows the task |
| `stop 1` | `task 1 stopped`, then `no running tasks`; socket closed |
| `stop 99` | `no task 99`, rc=1 |

The Windows DLL has **not** been executed here — no mingw on the dev host.
Validate with `make sliver` on Kali and a real session before relying on it.

## Known limitations

- **Windows only.** Linux was dropped: Sliver's Linux loader
  (`reflektor/native`) rejects Go `c-shared` images with
  `ErrGoSharedLibraryUnsupported`.
- **`make lint` runs before the build.** `windows_binaries` depends on `lint`;
  a pre-existing upstream vet failure in `pkg/proxy/netinfo` on newer Go will
  block `make sliver`. Fix or skip the dependency if that happens.
- **Proxy/relay not included.** Only `cmd/agent` is built. The relay still has
  to be run separately, and the agent needs a route back to it.
- **Agent reconnect loop:** with default settings the agent retries the initial
  connection only if `-retry` is passed; otherwise an unreachable relay is
  reported immediately (which is what makes the 2s error window useful).

## OPSEC notes

Same trade-offs as chisel's in-process extension:

- Agent code and strings live in the implant's memory.
- A second long-lived connection originates from the implant PID.
- A panic in the agent's per-connection goroutines is fatal to the implant; the
  entrypoint only recovers panics on its own goroutine.
- ligolo already defaults `-ua` to a Chrome User-Agent for websocket mode, which
  is better than a Go default. Prefer `https://` websocket mode and
  `-accept-fingerprint` over `-ignore-cert`.

If OPSEC outweighs convenience, run the agent as a separate process.

## Do not

- Do not modify the proxy (`cmd/proxy`) for Sliver.
- Do not call `logrus.Fatal` or `os.Exit` from anything the extension reaches.
- Do not store the Sliver callback pointer beyond the entrypoint call.
- Do not reintroduce `alias.json` or `.exe` payloads — both hit the problems
  described above.
- Do not add Linux `.so` targets without first verifying Sliver's current Linux
  loader accepts Go c-shared images.
