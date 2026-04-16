# linux-system-monitor

A Go daemon that monitors CPU usage and temperature, sends email alerts when
thresholds are sustained, and watches its own resource usage so it can't become
the problem it's watching for. Runs in a Podman container.

## What it does

- Polls CPU usage and temperature every configurable interval
- Alerts only when a threshold is **sustained** for N minutes — ignores transient spikes
- Cooldown between alerts to prevent spam during sustained events
- Only reads **CPU thermal zones** — battery, NVMe, PCH, and other non-CPU sensors are ignored
- Writes alert files to a drop directory; `entrypoint.sh` delivers them via `send-report`
- Self-safeguard: exits cleanly if the daemon itself exceeds CPU or RAM limits

## Quick start

### 1. Create host directories

```bash
mkdir -p ~/.config/linux-system-monitor
mkdir -p ~/.local/share/linux-system-monitor/{alerts,logs}
cp config.yaml ~/.config/linux-system-monitor/config.yaml
```

### 2. Build the image

```bash
podman build -t linux-system-monitor .
```

### 3. Run

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

### Useful commands

```bash
podman logs -f linux-monitor                                    # live logs
podman stop linux-monitor                                       # clean shutdown
ls -lt ~/.local/share/linux-system-monitor/alerts/             # alert history
cat ~/.local/share/linux-system-monitor/logs/monitor.log       # log file on host
```

### Debug build

Includes `bash`, `curl`, and `strace` for interactive inspection:

```bash
podman build --target=debug -t linux-system-monitor-debug .
podman run -it --rm linux-system-monitor-debug sh
```

## Configuration

Edit `~/.config/linux-system-monitor/config.yaml` and restart the container.

| Key | Default | Description |
|-----|---------|-------------|
| `cpu_usage_threshold_percent` | 90 | Alert threshold (%) |
| `cpu_temp_threshold_c` | 85 | Alert threshold (°C) — CPU zones only |
| `sustain_minutes` | 5 | How long condition must last before alerting |
| `check_interval_seconds` | 60 | Poll interval |
| `alert_cooldown_minutes` | 30 | Minimum minutes between alerts |
| `alert_dir` | `/alerts` | Drop directory (must match `--volume`) |
| `log_file` | `/logs/monitor.log` | Log path (must match `--volume`) |

Config path resolution order: `$MONITOR_CONFIG` → `/config/config.yaml` → `./config.yaml`

## How alerting works

The Go binary never calls `send-report` directly. Instead:

1. Threshold sustained → binary writes `/alerts/YYYY-MM-DD_HH-MM-SS_<trigger>.txt`
2. `entrypoint.sh` runs `inotifywait` in the background watching `/alerts/`
3. On each new file, the watcher pipes the body to `send-report`

Alert filenames encode which threshold(s) triggered:

```
2026-04-16_14-32-00_cpu+temp.txt   # both thresholds exceeded
2026-04-16_14-32-00_cpu.txt        # only CPU usage exceeded
2026-04-16_14-32-00_temp.txt       # only temperature exceeded
```

A hung `send-report` cannot block or crash the monitor — they run independently.

## Requirements

- `~/bin/send-report` — executable script (piped stdin → email via msmtp)
- `~/.msmtprc` — msmtp config with a `gmail` account defined
- `/sys/class/thermal` — present on physical hardware; absent in VMs (temperature monitoring disabled automatically, no false alerts)
- Podman with `--pid=host` support (standard on Linux)

## Running tests

```bash
go test ./... -v
```
