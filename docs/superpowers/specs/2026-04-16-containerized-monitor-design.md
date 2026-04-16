# Containerized Linux System Monitor — Design Spec

**Date:** 2026-04-16
**Status:** Approved
**Repo:** tom2025b/linux-system-monitor

---

## What We're Building

A complete rewrite of the linux-system-monitor daemon to run safely inside a
Podman container. The rewrite fixes all bugs identified in the April 2026 code
review and changes the alert delivery model so the Go binary never directly
executes host-side commands.

---

## Goals

- Run the monitor in an isolated Podman container (no --privileged)
- Fix all 7 bugs from the code review
- Alert delivery that cannot block or hang the monitoring goroutine
- XDG-compliant host paths for config, logs, and alerts
- Single `podman run` one-liner to start everything (systemd unit later)

---

## Files

```
linux-system-monitor/
├── Containerfile         # multi-stage: Go builder → Alpine final + debug target
├── entrypoint.sh         # PID 1: inotify watcher (bg) + Go monitor (fg)
├── main.go               # rewritten daemon — all bugs fixed
├── config.yaml           # mountable config with full comments
└── README.md             # updated with container instructions
```

The old `linux_system_monitor.yaml` is replaced by `config.yaml`.

---

## Container Architecture

### Build stages

**Stage 1 — builder** (`golang:1.22-alpine`)
- `CGO_ENABLED=0 GOOS=linux go build -o linux-system-monitor .`
- Produces a fully static binary with no libc dependency

**Stage 2 — debug** (`alpine:3.19`, target: `debug`)
- Adds `bash`, `curl`, `strace` for interactive troubleshooting
- Built only with `podman build --target=debug`
- Never used in production

**Stage 3 — final** (default, `alpine:3.19`)
- `apk add --no-cache msmtp inotify-tools`
- Copies binary from builder and `entrypoint.sh`
- `ENTRYPOINT ["/entrypoint.sh"]`
- Runs as root (required for msmtp to read `/root/.msmtprc`)

### Why Alpine (not distroless)

The inotify watcher (`entrypoint.sh`) requires a shell and `inotifywait`.
`msmtp` is also needed inside the container since `send-report` calls it.
Distroless/static cannot support these requirements.

---

## Runtime Mounts

| Purpose           | Host path                                              | Container path                    | Mode |
|-------------------|--------------------------------------------------------|-----------------------------------|------|
| Config file       | `~/.config/linux-system-monitor/config.yaml`          | `/config/config.yaml`             | ro   |
| Alert drop dir    | `~/.local/share/linux-system-monitor/alerts/`         | `/alerts/`                        | rw   |
| Log directory     | `~/.local/share/linux-system-monitor/logs/`           | `/logs/`                          | rw   |
| send-report script| `~/bin/send-report`                                   | `/usr/local/bin/send-report`      | ro   |
| msmtp config      | `~/.msmtprc`                                          | `/root/.msmtprc`                  | ro   |
| Thermal sensors   | `/sys/class/thermal`                                  | `/sys/class/thermal`              | ro   |

### Why `--pid=host`

gopsutil's `process.CPUPercent()` reads `/proc/<pid>/stat`. Inside a container
without `--pid=host`, the process sees container-namespaced PIDs that don't
match `/proc` on the host. `--pid=host` ensures the daemon's own PID is visible
in `/proc` for accurate self-monitoring.

---

## Alert Delivery (Drop Directory Model)

### Old model (broken)
```
Go binary → exec.Command("send-report") → blocks goroutine → hangs daemon
```

### New model
```
Go binary → writes /alerts/YYYY-MM-DD_HH-MM-SS_<type>.txt
entrypoint.sh (inotifywait loop) → reads file → pipes to send-report → msmtp
```

The Go binary never executes a subprocess. A hung `send-report` cannot block
monitoring. Alert files persist on the host as a natural audit trail.

### Alert filename convention

The filename encodes which threshold(s) triggered:
- `2026-04-16_14-32-00_cpu.txt` — only CPU usage exceeded threshold
- `2026-04-16_14-32-00_temp.txt` — only temperature exceeded threshold
- `2026-04-16_14-32-00_cpu+temp.txt` — both exceeded simultaneously

---

## Bug Fixes in main.go

| # | Bug | Fix |
|---|-----|-----|
| 1 | `sendAlert()` blocks daemon with no timeout | Eliminated — Go writes a file; watcher delivers it |
| 2 | `lastAlert` only set on success; no cooldown on failure | Eliminated — file write is atomic and instant |
| 3 | Alert subject shows both values regardless of which triggered | Filename + body explicitly name the triggering threshold(s) |
| 4 | `readMaxThermalTemp()` reads all zones including battery/NVMe | Filters by zone `type` file: only `x86_pkg_temp`, `cpu-thermal`, `cpu0-thermal`, `soc-thermal` |
| 5 | `selfHighStart` not reset when `selfErr` occurs | Reset to `time.Time{}` in the `selfErr` branch |
| 6 | Config loaded from CWD — fragile for daemon deployment | Checks `$MONITOR_CONFIG` → `/config/config.yaml` → `./config.yaml` |
| 7 | Unsafe `append` on config slice (`SendReportCmd`) | Field removed; alert delivery no longer uses a command |

### Additional changes

- `send_report_cmd` config key removed (no longer needed)
- `alert_dir` config key added (default: `/alerts`)
- `log_file` config key updated (default: `/logs/monitor.log`)
- Config validation updated accordingly

---

## Config Schema (config.yaml)

```yaml
cpu_usage_threshold_percent: 90
cpu_temp_threshold_c: 85
sustain_minutes: 5
check_interval_seconds: 60
alert_cooldown_minutes: 30

self_max_cpu_percent: 20
self_max_mem_mb: 100
self_sustain_seconds: 120
max_consecutive_errors: 5

alert_dir: /alerts
log_file: /logs/monitor.log
```

---

## entrypoint.sh Behaviour

```
1. Verify /alerts and /logs dirs exist (create if missing)
2. Start inotifywait loop in background:
   - Watches /alerts/ for close_write events
   - On new file: reads filename to extract subject, pipes body to send-report
3. exec Go binary in foreground (replaces shell as PID 1)
```

Using `exec` for the Go binary means it receives SIGTERM directly from Podman
on `podman stop`, enabling clean shutdown via the existing signal handler.

---

## podman run Command

```bash
podman run -d \
  --name linux-monitor \
  --pid=host \
  --volume ~/.config/linux-system-monitor/config.yaml:/config/config.yaml:ro \
  --volume ~/.local/share/linux-system-monitor/alerts:/alerts \
  --volume ~/.local/share/linux-system-monitor/logs:/logs \
  --volume ~/bin/send-report:/usr/local/bin/send-report:ro \
  --volume ~/.msmtprc:/root/.msmtprc:ro \
  --volume /sys/class/thermal:/sys/class/thermal:ro \
  linux-system-monitor
```

### Useful companion commands

```bash
# View live logs
podman logs -f linux-monitor

# Check alert files on host
ls -lt ~/.local/share/linux-system-monitor/alerts/

# Stop cleanly
podman stop linux-monitor

# Debug build (has shell)
podman build --target=debug -t linux-system-monitor-debug .
podman run -it --rm linux-system-monitor-debug sh
```

---

## Out of Scope (this iteration)

- systemd user unit (planned for after manual testing confirms everything works)
- Multi-architecture builds (linux/amd64 only)
- Metrics export (Prometheus, etc.)
- Alert deduplication across restarts
