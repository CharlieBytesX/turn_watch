# turn_watch

**English** · [Español](README.es.md)

Keep Rust builds — and everything your AI coding agents launch — from eating your
laptop. `turn_watch` shows every `cargo`/`rustc` build running on the machine (also
inside `docker build`) and everything started by coding agents (Claude Code, Codex,
opencode, aider, Gemini CLI…), grouped by project, git worktree and branch, and puts
a **core limit** on all of it:

- **Core queue**: all builds share one pool of N "turns" (a GNU make jobserver).
  Five parallel `cargo test` runs from five agents use N cores between them, not
  5 × all cores.
- **Pause**: compile jobs over the limit (`rustc`, `cc1`, build scripts — even ones
  that ignore the queue) are paused with `SIGSTOP` and resumed in order with
  `SIGCONT`. Oldest first; nothing is ever left paused.
- **CPU cap** (Linux): builds and agents run inside a systemd slice with
  `CPUQuota = N × 100%`.
- **Build cache**: installs and configures [kache](https://kunobi.ninja/docs/kache)
  so rebuilt crates come from cache.
- A terminal panel, desktop notifications when long builds finish, and a bar widget
  on [Omarchy](https://omarchy.org).

The limit is kept by a small background service, so it stays on after you close the
panel until you turn it off. The tool's interface (panel, messages) is in Spanish.

> **macOS support is new** and so far tested only in CI (GitHub Actions macOS
> runners). On macOS the limit is the queue and the pause only: there are no
> cgroups, so there is **no CPU cap** — see [macOS](#macos).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/CharlieBytesX/turn_watch/main/install.sh | sh
```

The script detects your OS and architecture (Linux/macOS, amd64/arm64), downloads
the latest release, verifies its SHA-256 against the release's `SHA256SUMS`, puts
the binary in `~/.local/bin` and runs `turn_watch install`, which sets up:

- `~/.local/bin/turn_watch` and `~/.local/bin/cargo` → `turn_watch` (the cargo
  wrapper; see [FAQ](#why-a-cargo-wrapper));
- the background service: `turn_watch.service` + `turn_watch.slice` (systemd user
  units) on Linux, a LaunchAgent on macOS;
- kache (found, or installed with `cargo-binstall` or from its official GitHub
  release, SHA-256 checked) and `kache init`, which sets `rustc-wrapper` in
  `~/.cargo/config.toml`. Skip it with `TURN_WATCH_SIN_KACHE=1`;
- on Omarchy, the bar widget.

`~/.local/bin` must come **before** `~/.cargo/bin` in your `PATH`; the installer
warns if it does not.

Options: `TURN_WATCH_VERSION=v0.1.0` installs a given tag; `TURN_WATCH_NO_SETUP=1`
only drops the binary. Or download a tarball from
[Releases](https://github.com/CharlieBytesX/turn_watch/releases) and run
`./turn_watch install` yourself. From source (Go, see `go.mod`):

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' && ./turn_watch install
```

## Usage

```sh
turn_watch            # open the panel (turns the limit on if it was off)
```

The panel shows CPU and RAM split into *builds / agents / everything else*, the
queue, and one row per build and per agent project/worktree. Keys:

| Key | Action |
| --- | --- |
| `↑` `↓` / `k` `j` / mouse wheel | select a row |
| `Enter` | expand / collapse its details |
| `+` / `-` | raise / lower the limit (applies immediately) |
| `d` / `a` | turn the limit off / on |
| `s` | whole-system mode (asks for sudo) |
| `t` | toggle the agents' CPU cap (Linux) |
| `q`, `Esc`, `Ctrl-C` | close the panel (the limit stays) |

Commands:

| Command | What it does |
| --- | --- |
| `turn_watch on` / `off` | turn the limit on / off without the panel |
| `turn_watch limit [N]` | show or change the limit (cores), live |
| `turn_watch sistema on\|off` | `on`: also limit root, Docker and other users (sudo); `off`: only your user |
| `turn_watch hold N -- cmd…` | reserve N turns of the queue while `cmd` runs (for things that don't speak the jobserver protocol, like `docker build`) |
| `turn_watch once` | print the current state once, as plain text |
| `turn_watch install` / `uninstall` | set up / remove everything for the current user |
| `turn_watch version` | print the version |
| `turn_watch help` | full help (Spanish) |

The default limit is all cores minus two. Configuration lives in
`config.json` (`~/.config/turn_watch/` on Linux,
`~/Library/Application Support/turn_watch/` on macOS):

```json
{
  "slots": 10,
  "active": true,
  "system": false,
  "agents": ["claude", "codex", "opencode", "pi", "aider", "gemini", "cursor-agent", "goose"],
  "agents_cap": true
}
```

`agents` is the list of process names treated as coding agents; `agents_cap: false`
leaves agents uncapped (Linux).

## How it works

More detail in [docs/how-it-works.md](docs/how-it-works.md).

- **Monitor.** Once a second the service reads the process table (`/proc` on Linux;
  `sysctl` + one `ps` call on macOS), finds `cargo` roots and their `rustc`, build
  scripts and linkers, and the agents' process trees. Each build gets its
  project, worktree and branch from git; each agent subtree is grouped by the
  working directory of its processes. The state goes to
  `$XDG_RUNTIME_DIR/turn_watch/state.json` for the panel and the bar.
- **Queue (jobserver).** While the limit is on, the service owns a FIFO with N−1
  tokens (`$XDG_RUNTIME_DIR/turn_watch/jobserver`). The cargo wrapper passes it to
  the real cargo as `CARGO_MAKEFLAGS=--jobserver-auth=fifo:…`, so every `rustc` takes
  a token before starting and gives it back when done — across all concurrent
  builds. Lowering the limit takes tokens back as they are returned.
- **Pause.** Five times a second the service counts compile jobs under any cargo and
  pauses (`SIGSTOP`) the newest ones above the limit, resuming them (`SIGCONT`)
  when there is room. This also catches builds that bypass the wrapper. Paused
  PIDs are saved to disk and resumed if the service dies. Without sudo it can only
  pause your own processes; with whole-system mode a small root helper
  (`sudo turn_watch enforce`) does it for every user, including Docker's.
- **CPU cap (Linux).** The wrapper runs cargo inside `turn_watch.slice`
  (`systemd-run --user --scope`), and the service sets the slice's `CPUQuota` to
  N × 100 %. While the limit is on, agents and all their descendants are moved
  (via systemd's D-Bus API, nothing is restarted) into
  `turn_watch.slice/turn_watch-agentes.scope`, so whatever they launch later is
  born there and shares the same cap.

### macOS

macOS has no `/proc` and no cgroups. What changes:

- Processes come from `sysctl kern.proc.all`, CPU and memory from one `ps` call per
  second, and exact arguments from `sysctl kern.procargs2`. A process's working
  directory is read with `lsof`, which is slow, so only for roots (cargo and agents)
  and cached; child processes are shown under their parent.
- **The limit is the queue and the pause only. There is no CPU cap**, neither for
  builds nor for agents (agents are shown, not limited). The cargo wrapper also
  lowers cargo's priority (`nice 10`) while the limit is on; that only yields CPU to
  other programs under contention, it does not cap anything.
- The service is a LaunchAgent (`~/Library/LaunchAgents/io.github.charliebytesx.turn_watch.plist`,
  log in `~/Library/Logs/turn_watch.log`); notifications use `osascript`. State
  lives in `~/Library/Caches/turn_watch/`.
- Docker Desktop builds run inside its Linux VM: they are neither visible nor
  limited.
- No Omarchy widget, obviously.

## Requirements

- Linux (amd64/arm64) with a systemd user session for the service and the CPU cap
  (without systemd you can run `turn_watch daemon` yourself; you get the queue and
  the pause, no cap), or macOS (Apple Silicon/Intel).
- A recent Rust toolchain: cargo must understand `--jobserver-auth=fifo:` (any
  2024 or later release does).
- `sudo` only for whole-system mode.

## Uninstall

```sh
turn_watch uninstall
rm ~/.local/bin/turn_watch
```

This removes the service, the cargo wrapper (restoring a previous
`~/.local/bin/cargo` if there was one) and the bar widget. Your configuration and,
if you want to drop kache too, the `rustc-wrapper = "kache"` line in
`~/.cargo/config.toml` are left for you to delete.

## FAQ

### Why does it want sudo?

Only for whole-system mode (`turn_watch sistema on` or `s` in the panel). Without
it turn_watch can only pause your own processes. Builds run as root — Docker
BuildKit, `sudo cargo` — or by other users need a root helper to be paused. The
helper only pauses and resumes compile jobs and exits when the limit is turned
off. Everything else runs as your user.

### Does it work with Docker?

On Linux the `cargo` inside `docker build` is visible (the project is taken from the
`Cargo.lock` where you ran `docker build`). It runs as root, so it is paused only
in whole-system mode, and it does not use the jobserver; wrap the build with
`turn_watch hold N -- docker build …` to reserve N turns for it. On macOS Docker
builds run inside a VM and are invisible.

### Why a cargo wrapper?

The queue only works if cargo is given the jobserver, and the CPU cap only if cargo
starts inside the slice. A `~/.local/bin/cargo` symlink to `turn_watch` does both
without aliases or shell config: when the limit is on it sets `CARGO_MAKEFLAGS`
and, on Linux, runs cargo via `systemd-run --scope`; when it is off it just
`exec`s the real cargo (the first one in `PATH` that isn't itself, or
`~/.cargo/bin/cargo`). Builds that bypass it are still caught by the pause.

### Does closing the panel turn the limit off?

No. The service keeps it until `turn_watch off` or `d` in the panel. Opening the
panel turns it on.

## License

[MIT](LICENSE)
