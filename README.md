# wudo

[![CI](https://github.com/italoalmeida0/wudo/actions/workflows/ci.yml/badge.svg)](https://github.com/italoalmeida0/wudo/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-windows%20arm64%20%7C%20amd64-lightgrey)](#requirements)

`wudo` is a Windows equivalent of Linux `sudo`, built for AI agent harnesses and
for anyone who wants a terminal that can run elevated commands on demand.

It is two small executables, plus a one-file installer:

| Binary            | Runs as          | What it does                                                         |
|-------------------|------------------|----------------------------------------------------------------------|
| `wudosvc.exe`     | administrator    | Sits in the notification area, stays **inactive**, and runs the commands sent to it once you activate it for a while. Starts with Windows, elevated, with no UAC prompt and no console window. |
| `wudo.exe`        | your normal user | Sends one command to the service and streams its output back as if it had run locally |
| `wudoinstall.exe` | administrator    | Installs everything (and uninstalls it again)                          |

There is no password, no token and no prompt per command: **the service being
active is the authorisation**. That is the whole point - an agent (or a script)
can call `wudo` freely while the service is active, and the moment it is
deactivated every request is refused and every running command is terminated.

```
  your terminal / agent harness                elevated service (tray)
  ────────────────────────────                 ──────────────────────
  wudo net stop wuauserv   ─── WebSocket ───▶  run cmd.exe in a job object
        ▲                    127.0.0.1              │
        └──── stdout/stderr/exit code ◀─────────────┘
```

## Why two binaries

Anything that can elevate a process on Windows has to start from an elevated
process. `wudosvc.exe` is that process: you start it once, as administrator, and
it stays around in the tray. `wudo.exe` never needs any privilege at all, so an
agent can start it as often as it likes.

`wudo.exe` is a **thin client**: it does not implement any part of the privilege
boundary, it only speaks HTTP/WebSocket to `127.0.0.1`.

## Download

Grab `wudoinstall.exe` from the [releases page](https://github.com/italoalmeida0/wudo/releases)
(choose `arm64` for Windows on ARM, `amd64` for Intel/AMD) and run it. Building
from source is described [below](#building-from-source).

## Requirements

* Windows 10 1809 (build 17763) or newer - the pseudo console API (ConPTY) is
  needed for interactive commands. Older builds still work for non interactive
  commands, which use plain pipes.
* No runtime dependency: both binaries are static Go executables.

## Install

Run `wudoinstall.exe` and accept the UAC prompt. It does four things:

1. copies `wudo.exe`, `wudosvc.exe` and `wudo-uninstall.exe` to `C:\wudo\bin`;
2. appends `C:\wudo\bin` to the **machine** `PATH` (no duplicate on reinstall),
   then broadcasts `WM_SETTINGCHANGE` so new shells see it immediately;
3. registers the scheduled task **wudo-service**: trigger *at logon*, principal
   *your account*, *RunLevel = HighestAvailable*, *LogonType = InteractiveToken*.
   This is what makes the service start elevated at every logon **without a UAC
   prompt** - the consent was given once, when you ran the installer. The task
   also gives the tray icon a chance to appear in your session, which a Windows
   *service* in session 0 cannot do;
4. starts the service right away through that task.

`wudosvc.exe` is linked as a **GUI subsystem** binary, so it never opens a console
window - it lives only in the notification area. Its log goes to
`C:\wudo\wudosvc.log`.

Installer options (all optional):

```
wudoinstall.exe                      interactive install
wudoinstall.exe --yes                no questions (good for scripts)
wudoinstall.exe --root D:\wudo       install somewhere else
wudoinstall.exe --no-path            do not touch PATH
wudoinstall.exe --no-autostart       do not register the logon task
wudoinstall.exe --no-start           install but do not start the service
wudoinstall.exe --uninstall          remove wudo
wudo-uninstall.exe                   same thing, keep the log (--keep-data)
```

The installer writes `C:\wudo\install.log`, so a failed run can be diagnosed
afterwards. If you prefer not to install anything, the three binaries in
`bin\<arch>\` can be copied by hand and the service started with *Run as
administrator*; only the no-UAC autostart needs the scheduled task.

## Quick start

1. Install (above). Open a **new** terminal so the `PATH` change is picked up.
2. The service is already running, greyed out in the notification area, meaning it
   is inactive.
3. Right click the tray icon → *Activate* → *For 1 hour* (or 2, 3, 6, 12, 24).
   The icon turns green.
4. Use it:

   ```bat
   wudo whoami /priv
   wudo ipconfig /flushdns
   wudo -t powershell -NoLogo
   wudo --shell "net stop wuauserv && net start wuauserv"
   ```

5. When you are done, right click the tray icon → *Deactivate*. It quits
   immediately, and so does every command it was running.

Because `wudo` behaves like a normal command, an agent harness can use it
transparently: the exit code is the command's exit code, stdout and stderr stay
separate, and the working directory is the caller's.

## The tray menu

```
Status                       Inactive                       (or "Active until 17:45 (2h 05m left)")
Activate                     ▶ For 1 hour
                               For 2 hours
                               For 3 hours
                               For 6 hours
                               For 12 hours
                               For 24 hours
No commands running
Open log file
State folder
Version                      wudo 0.1.0 - port 51234 - elevated
Deactivate
Quit
```

* The icon is grey while inactive and green while active.
* The status line and the tooltip show a live countdown.
* The number of running commands is shown in the menu, so you can tell whether
  an agent is still working.
* *Deactivate* refuses new commands and terminates the running ones.
* *Quit* stops the service completely and removes its port file.

The service always starts **inactive**, even if it is launched at logon, so
nothing can run elevated until a human activates it.

## Command line

```
wudo [options] <command> [args...]
wudo [options] --shell "<command line>"

  -t, --tty             use a pseudo console (automatic when stdout is a terminal)
      --no-tty          always use plain pipes
      --shell           run the whole command line through cmd.exe
      --timeout <dur>   kill the command after this long (30s, 5m, 1h, or bare seconds)
      --cwd <dir>       working directory for the command (default: the caller's)
      --status          show the state of the service and exit
      --version         print the version and exit
      --verbose         print diagnostics on stderr
  -h, --help            show the help

Environment:
  WUDO_TTY=0|1          force the pseudo console off or on
  WUDO_DIR=<dir>        use a different state folder (default C:\wudo)
```

Environment variables understood by `wudosvc.exe` (all optional):

| Variable | Effect |
|----------|--------|
| `WUDO_DIR` | State folder, default `C:\wudo` |
| `WUDO_AUTOACTIVATE` | Start already active for this long (`10m`, `1h`, …). Off by default - the service starts inactive. |
| `WUDO_IDLE_TIMEOUT` | Deactivate automatically after this long without any command (`15m`, `1h`, …) |
| `WUDO_QUIET=1` | Never show message boxes (used by the test suite) |
| `WUDO_KEEPALIVE` | Keepalive ping interval, default `15s` |

Exit codes:

| Code | Meaning                                                          |
|------|------------------------------------------------------------------|
| 0-N  | the command's own exit code                                      |
| 124  | `--timeout` expired                                              |
| 130  | the command was interrupted (Ctrl+C)                             |
| 143  | the command was terminated                                       |
| 1    | the service is not running, not active, or the command failed to start |

When the service is not running or not active, `wudo` prints:

```
wudo: service not active
wudo: right-click the wudo tray icon and choose Activate.
```

## Transparency details

These are the things that usually break when a command is executed remotely; all
of them are handled:

* **Working directory** - the client sends its `cwd` and the service runs the
  command there.
* **Environment** - the caller's `PATH`, `PATHEXT`, `USERPROFILE`, `TEMP` and the
  user identity variables are applied on top of the service's environment, while
  `SystemRoot`, `windir`, `SystemDrive` and `ComSpec` always describe the elevated
  environment (so elevation-sensitive binaries still load correctly).
* **`PATH`/`PATHEXT` resolution** - performed in the client's directory *and*
  against the caller's `PATH`, exactly like `cmd.exe` would, so `wudo npm` finds
  `npm.cmd` even though the service has a different `PATH`.
* **`.cmd`/`.bat` shims** - detected and executed through
  `cmd.exe /d /s /c "…"` with the argument quoting `cmd.exe` expects. This is
  what makes `wudo npm`, `wudo yarn`, `wudo gradlew.bat`, … work.
* **Interactive TTY** - with `-t` (or automatically when stdout is a terminal)
  the command runs inside a ConPTY: colours, cursor movement, `Ctrl+C`, window
  resizes and full-screen programs work. The client puts its own terminal in raw
  mode, enables virtual terminal input/output, and forwards resize events.
* **Streaming** - output is forwarded in 32 KiB frames as it is produced, never
  buffered until the end, so long builds show progress immediately.
* **Exit codes** - the real Windows exit code is returned; statuses above 255 are
  truncated to their low byte because process exit codes are bytes.
* **Ctrl+C** - the first `Ctrl+C` is forwarded to the command as an interrupt
  (`0x03` inside the pseudo console, `TerminateJobObject` for pipe mode); the
  second one abandons it, which makes the client disconnect and the service kill
  the process tree.

## Disconnects

Both directions are handled, in both directions of failure:

* **The client disappears** (process killed, terminal closed, agent harness
  aborted) - the WebSocket write fails or the next output frame cannot be
  delivered, the service abandons the session, sets the reason to `terminated`
  and kills the job object, so the whole elevated process tree goes with it. A
  half-open connection is caught by the keepalive: the service pings the client
  every 15 s and two missed pongs are treated exactly like a disconnect.
* **The service disappears** (killed from Task Manager, tray *Quit*, crashed,
  machine shut down) - the client's read fails, or its own keepalive ping times
  out, and `wudo` exits with code 1 and a message instead of waiting forever. The
  job object is closed by the OS when the service dies, which kills the commands
  it was running (that is what `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` is for).

The behaviour is covered by `scripts\disconnect.ps1`, which kills a client and
then a service while commands are running and checks that nothing survives.

## Concurrency and isolation

* Every command gets **its own Job Object** with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. A hung command, a crashed child or a
  runaway process tree can always be killed as a unit, and closing the job
  guarantees no elevated orphan is left behind.
* Each command runs in its **own goroutines and its own pseudo console/pipes**;
  up to 64 commands run at the same time (`MaxSessions`), and one of them being
  slow, hung or noisy cannot affect another - or the service.
* Deactivating, quitting or losing the client connection terminates that command
  and every process it started.
* The service **never blocks** on a command: HTTP is served by a normal Go server
  and each WebSocket runs in its own handler goroutine.

## Security model

* The HTTP/WebSocket server binds to `127.0.0.1` on an ephemeral port and refuses
  requests that carry an `Origin` header, so a web page in a local browser cannot
  drive it.
* The port and the service PID are published in `C:\wudo\endpoint.json`. The
  folder's ACL is `SYSTEM` and `Administrators: full`, `Users: read` - the
  discovery file is readable (the client needs it), but nothing in it can be
  modified by an unprivileged user.
* When `C:\wudo` cannot be created (the service was started without elevation),
  the service falls back to `%LOCALAPPDATA%\wudo`.
* There is intentionally **no authentication**: the activation window is the
  authorisation. Treat *Activate* like `sudo`'s timestamp - open it when you need
  it, close it when you are done. The service always starts inactive and can be
  activated for at most 24 hours at a time.
* The service writes an audit log to `C:\wudo\wudosvc.log` with the command line,
  the working directory, the start/exit of every session and the activation
  changes.

## Files on disk

```
C:\wudo\
  endpoint.json    discovery: schema, port, pid, instance, version, user, start time
  wudosvc.log      audit log (rotated at 1 MiB)
  install.log      what the installer and the uninstaller did
  bin\
    wudo.exe
    wudosvc.exe
    wudo-uninstall.exe
```

The service recreates the two runtime files on every start. Uninstalling removes
the whole folder; `--keep-data` keeps the log.

## Building from source

Requirements: Go 1.23+ (any recent version works; the project was developed with
Go 1.27), and `rsrc` for the embedded icons/manifests.

```powershell
powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Arch arm64
powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Arch amd64
```

Each run produces `bin\<arch>\wudo.exe`, `wudosvc.exe` and `wudoinstall.exe`.

The script installs `rsrc` if needed, compiles the `.ico`/manifest resources for
the requested architecture, builds the client and the service, and then embeds
both into the installer. `-OutDir` picks another output folder. Add
`-NoElevationManifest` to build a service and installer that can be started from
an ordinary shell (debugging and the test scripts only - their commands are not
elevated).

Manual equivalent:

```
go install github.com/akavel/rsrc@latest
rsrc -arch amd64 -ico cmd\wudo\wudo.ico -manifest cmd\wudo\wudo.exe.manifest -o cmd\wudo\rsrc_windows_amd64.syso
rsrc -arch amd64 -ico cmd\wudosvc\wudosvc.ico -manifest cmd\wudosvc\wudosvc.exe.manifest -o cmd\wudosvc\rsrc_windows_amd64.syso
rsrc -arch amd64 -ico cmd\wudoinstall\wudoinstall.ico -manifest cmd\wudoinstall\wudoinstall.exe.manifest -o cmd\wudoinstall\rsrc_windows_amd64.syso
go build -o bin\amd64\wudo.exe .\cmd\wudo
go build -ldflags -H=windowsgui -o bin\amd64\wudosvc.exe .\cmd\wudosvc
copy bin\amd64\wudo.exe cmd\wudoinstall\payload\
copy bin\amd64\wudosvc.exe cmd\wudoinstall\payload\
go build -tags wudo_installer -o bin\amd64\wudoinstall.exe .\cmd\wudoinstall
```

`wudosvc.exe` and `wudoinstall.exe` embed a manifest with
`requireAdministrator`, so Windows asks for UAC consent when they are started by
hand. `wudo.exe` uses `asInvoker` and never triggers a prompt. The service is
linked with `-H windowsgui` so it has no console window at all.

The installer embeds the other two binaries, which is why the payload folder has
to be staged before it is compiled; `scripts\build.ps1` does that automatically.

### Layout

```
cmd/wudo/            client CLI
cmd/wudosvc/         elevated tray service (manifest + icon live here)
cmd/wudoinstall/     installer and uninstaller (embeds the other two binaries)
internal/setup/      PATH, scheduled task, install/uninstall logic
internal/conpty/     ConPTY wrapper over golang.org/x/sys/windows
internal/runner/     process creation, job objects, output multiplexing
internal/server/     HTTP status endpoint + WebSocket command handler
internal/client/     WebSocket client, terminal handling
internal/protocol/   wire format shared by both sides
internal/spawn/      PATH/PATHEXT resolution and command line quoting
internal/state/      C:\wudo discovery folder
internal/tray/       notification area menu
internal/winproc/    small Win32 helpers (job objects, elevation, process state)
scripts/             build.ps1, smoke.ps1, disconnect.ps1, install-test.ps1
```

## Tests

```
go test ./...
powershell -ExecutionPolicy Bypass -File scripts\smoke.ps1
powershell -ExecutionPolicy Bypass -File scripts\disconnect.ps1
powershell -ExecutionPolicy Bypass -File scripts\install-test.ps1
```

`go test ./...` runs unit tests for command line quoting, PATH resolution, the
scheduled task definition, the PATH add/remove logic and the whole install and
uninstall file handling; a real ConPTY round trip; resize propagation; and
end-to-end tests that start a service on a loopback port and drive it through the
real client (pipes, TTY, stdin, concurrency, interrupts, deactivation, timeouts,
keepalive).

The three PowerShell scripts exercise the actual binaries:

| Script | What it checks |
|--------|----------------|
| `smoke.ps1` | inactive gate, exit codes, stdout/stderr split, TTY, stdin, cwd, `npm.cmd`, five concurrent commands, a long command surviving a short one, timeout |
| `disconnect.ps1` | killing the client kills its command; killing the service makes the client exit; no orphans left |
| `install-test.ps1` | install into a temporary folder, install twice, then uninstall with the installed uninstaller |

The service cannot be started from a non elevated shell because of its
`requireAdministrator` manifest, so the scripts build a manifest-less copy for the
duration of the run and restore the production resources afterwards. Commands
started by that copy are not elevated - the scripts therefore check behaviour, not
privileges.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| `wudo: service not active` | The service is not running or was not activated. Start `wudosvc.exe` as administrator and use the tray icon. |
| `wudo: service not reachable` | The service died (or was killed) but left its port file. Restart it; the client also cleans up stale files automatically. |
| Commands run but are not elevated | The tray tooltip and `wudo --status` say `NOT elevated`. Quit the service and start it again with *Run as administrator*. |
| `CreatePseudoConsole` errors | Windows older than 10 1809. Use `--no-tty`; interactive programs will not work but everything else will. |
| A command is stuck | Press `Ctrl+C` twice, or deactivate the service: the job object takes the whole process tree down. |
| `Another wudo service is already running` | A second copy was started. Use the tray icon of the first one. |
| The service does not start at logon | Check the task: `schtasks /Query /TN wudo-service /V /FO LIST`. Re-register it with `wudoinstall.exe --no-path`. |
| `wudo` is not found in a new terminal | The `PATH` change needs a new shell. The installer broadcasts the change; if the terminal was already open, open another one. |
| The uninstaller says "will be deleted at the next restart" | A command was still running from `C:\wudo\bin`. Windows deletes the file at the next boot. |

Debugging aids: `wudo --verbose …`, `wudo --status`, and the log at
`C:\wudo\wudosvc.log` (also reachable from the tray menu).

## License

MIT.
