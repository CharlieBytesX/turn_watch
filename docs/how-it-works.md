# How turn_watch works

[Español](como-funciona.md)

turn_watch is one static Go binary with several roles, chosen by its name and first
argument:

| Invoked as | Role |
| --- | --- |
| `turn_watch daemon` | the background service (owner of the limit) |
| `turn_watch` | the panel (TUI): only reads the service's state and sends it requests |
| `cargo` (symlink) | the cargo wrapper |
| `sudo turn_watch enforce --dir DIR` | the root helper (whole-system mode) |
| `turn_watch on/off/limit/hold/once/…` | small clients of the service |

## Files

Runtime directory: `$XDG_RUNTIME_DIR/turn_watch/` on Linux (or the temp dir if
unset), `~/Library/Caches/turn_watch/` on macOS.

| File | Written by | Purpose |
| --- | --- | --- |
| `state.json` | service, every second | everything the panel and the bar show |
| `jobserver` | service | the FIFO with the queue's tokens (exists only while the limit is on) |
| `queue.json` | service | current limit and service PID, for the wrapper and `hold` |
| `queue.lock` | service | `flock`: one queue owner at a time |
| `control` | `on`/`off`, panel | request to turn the limit on or off |
| `limit.request` | `limit N`, panel | request to change the limit live |
| `stopped.json` | service | PIDs it paused, resumed after a crash |

The root helper never writes into that (user-owned) directory: its files go to
`/run/turn_watch/<uid>/` (`/var/run/turn_watch/<uid>/` on macOS), root-owned and
mode 0755 so the service can read `helper.pid`:

| File | Written by | Purpose |
| --- | --- | --- |
| `stopped-root.json` | root helper | PIDs it paused, resumed after a crash |
| `helper.pid` | root helper | whether whole-system mode is running |

Every state file is written through `writeFileSafe` (the directory opened with
`O_DIRECTORY|O_NOFOLLOW` and checked on the descriptor: a real directory, owned by
the expected user, not group/other-writable; then a random temp name created with
`openat(O_CREAT|O_EXCL|O_NOFOLLOW)` and `renameat`) and read with `O_NOFOLLOW`, so
a symlink planted in a state directory can't redirect a write or a read. The root
helper also validates `--dir` (absolute, named `turn_watch`, not a symlink, owned
by `SUDO_UID`) and opens the queue's FIFO only if it is a FIFO owned by that user.

Configuration: `config.json` in `~/.config/turn_watch/` (Linux) or
`~/Library/Application Support/turn_watch/` (macOS). The service re-reads it every
second, so editing it by hand works.

## The service loop

Every second: read the process table, update builds and agent groups, apply
`control`/`limit.request`, (Linux) move agents into the capped scope, write
`state.json`, and send a desktop notification for builds that ran 20 s or more.
Every 100 ms it samples the `rustc` processes under each build (most crates compile
in under a second and would be missed otherwise), and every 200 ms it enforces the
pause.

### Builds

A build is a root `cargo` (one with no cargo above it) and everything under it. Its
project, git worktree and branch come from the root's working directory (walking up
to `.git`; worktrees are detected from the `gitdir:` file). Progress counts crates
that are *done*, compiled or restored from cache: the larger of (a) units whose
cargo fingerprint (`<target>/[<triple>/]<profile>/.fingerprint/*/*.json`, written by
cargo when a unit finishes, whatever the rustc wrapper did) is newer than the
build's start, and (b) crates seen in `rustc` or wrapper (`kache`, `sccache`)
processes with `--crate-name` (the only source for Docker builds). The target dir
is found like cargo does: `--target-dir`, `CARGO_TARGET_DIR`, `build.target-dir` in
`.cargo/config.toml`, else `<workspace>/target`; the phase (compiling, linking, build script, C/C++, fetching)
from the children. For Docker builds (Linux) the cargo runs as root inside BuildKit;
its project is taken from the `Cargo.lock` of the directory where your
`docker build`/`uc build` runs.

### Agents

Processes whose name (or `argv[0]`, or `argv[1]` for `node`/`bun`/… scripts) is in
`agents` are agent roots. Each process in their subtrees is assigned to the group of
its own working directory if that is a git repository, otherwise to its parent's.
CPU is the per-process delta between reads; on Linux the CPU of children that died
between reads is recovered from the parent's `cutime` without double counting.
Groups are smoothed (exponential moving average, ~3 s) and kept for 3 minutes after
their last process so the list doesn't jump around. Builds started by an agent are
shown once, as builds.

## The limit

**Queue.** The service creates the FIFO and writes N−1 tokens (`+` bytes); each cargo
has one implicit token, per the GNU make jobserver protocol. The wrapper sets
`CARGO_MAKEFLAGS="-jN --jobserver-auth=fifo:PATH"` and removes `CARGO_BUILD_JOBS`.
Raising the limit writes more tokens; lowering it registers a debt and a reader
takes tokens out as they come back. When nothing is building for 3 s the pool is
topped up (a process that died holding tokens loses them). Turning the limit off
writes plenty of tokens (whatever was waiting continues as an unlimited build) and
deletes the FIFO, so new cargos don't use it.

**Pause.** Jobs are `rustc`, `cc1`, `cc1plus` and `build-script-*` processes that
have a cargo among their ancestors. Sorted by start time, the first N run; the rest
get `SIGSTOP`. When a running one ends, the oldest paused one gets `SIGCONT`. The
service never pauses anything else (agents, tests, servers). Paused PIDs are stored
with their start time and owner, so a recycled PID is never signalled; on startup,
and when the root helper starts, leftovers from a crash are resumed — but only PIDs
that still have the recorded start time and owner, are stopped, and are a job
under a cargo (no cgroup check: the pause deliberately reaches builds outside the
slice, such as Docker's).

**Whole-system mode.** The panel asks for sudo once (`sudo -v` in the terminal) and
starts `sudo turn_watch enforce --dir DIR --detach`, which runs the same pause loop
as root for every user's jobs, reading the limit from `queue.json`, and exits when
the limit is turned off. The service then stops pausing on its own.

**CPU cap (Linux only).** `turn_watch.slice` is a systemd user slice
(`CPUWeight=50`). The service sets `CPUQuota=N×100%` on it while the limit is on and
clears it when off. The wrapper runs cargo with
`systemd-run --user --scope --slice=turn_watch.slice`. Agents are moved with
`StartTransientUnit` / `AttachProcessesToUnit` (D-Bus via `busctl`) into
`turn_watch-agentes.scope` inside that slice; processes outside your
`user@.service` (an SSH session, another user) cannot be moved and are reported
once in the log. With `agents_cap: false` they are moved out to
`app.slice/turn_watch-agentes-libre.scope`.

## kache

`turn_watch install` downloads kache at the version pinned in `kache.go`
(`kacheVersion`) and checks it against the SHA-256 pinned there for each platform;
`kache init -y --no-shell` runs only when cargo has no `rustc-wrapper` yet (read from
`RUSTC_WRAPPER`, `CARGO_BUILD_RUSTC_WRAPPER` and `$CARGO_HOME/config[.toml]`, parsed
as TOML). What fails is kept in `kache-setup-error.txt` next to `config.json`.

The service checks kache in a goroutine at startup and every 30 s:
`kache --json daemon status`, then `kache --json stats --since 24h` only if the
daemon is up, and `kache --json doctor` every 30 min. Each answer is kache's
versioned JSON document (`schema_version` 1); a missing field, another schema
version or `success: false` becomes an explicit "unavailable: why" state in
`state.json` (`kache`), never zeros.

kache's daemon (`kache.service`) is outside `turn_watch.slice` unless
`kache_in_slice` is set (a drop-in with `Slice=turn_watch.slice`, applied on the
next restart of `kache.service`). Compiles on a cache miss are started by the
`kache` client under cargo, so they are in the slice and under the pause either
way; kache's own scheduler can also hold compilers after misses.

## macOS

| Linux | macOS |
| --- | --- |
| `/proc/<pid>/stat`, `cmdline` | `sysctl kern.proc.all` (pid, ppid, uid, name, state, start) + `sysctl kern.procargs2` (exact argv) |
| CPU and RSS from `/proc` | one `ps -axww -o pid=,ppid=,uid=,rss=,%cpu=,time=,state=,args=` per second (not for the 200 ms pause loop) |
| `/proc/<pid>/cwd` for every process | `lsof -a -d cwd -Fn -p PID`, only for cargo and agent roots, cached per PID for 30 s |
| `cutime` of dead children | not available: CPU a process spent between the last read and its death is lost |
| machine CPU from `/proc/stat` | sum of the processes' `%cpu` from `ps` |
| memory from `/proc/meminfo` | `hw.memsize` + `vm_stat` (app + wired + compressed, like Activity Monitor) |
| `TIOCINQ` on the FIFO | `FIONREAD` |
| systemd user service | LaunchAgent (`launchctl bootstrap gui/$UID`) |
| `notify-send` | `osascript` (`display notification`) |
| slice + `CPUQuota` | **none**: queue and pause only; the wrapper sets `nice 10` on cargo |

The process code lives in `proc_linux.go` / `proc_darwin.go`, the service and cap in
`service_linux.go` + `agentcap_linux.go` / `service_darwin.go`, terminal ioctls in
`tty_*.go`. The output parsers for `ps`, `lsof`, `vm_stat` and `kern.procargs2` are
in `psparse.go` and are unit-tested on every platform.

## CI and releases

`.github/workflows/ci.yml` runs `go vet` and the tests on Ubuntu and macOS, a smoke
test of the real service on both (on macOS also `turn_watch install` with launchd
in a temporary `HOME`), and cross-compiles the four binaries. Pushing a `v*` tag
runs `.github/workflows/release.yml`, which builds them with `scripts/build.sh`
(`CGO_ENABLED=0`, `-X main.version=<tag>`), packs
`turn_watch-<os>-<arch>.tar.gz` plus `SHA256SUMS` and creates the GitHub release.
