# linux-system-monitor

A Go daemon that watches CPU usage and temperature, sends email alerts via `send-report` when thresholds are sustained, and monitors its own resource usage so it can't become the problem it's watching for.

## What it does

- Polls CPU usage and CPU temperature at a configurable interval
- Alerts (via `send-report`) only when a threshold is **sustained** for N minutes
- Cooldown between alerts to prevent spam
- Self-safeguard: exits cleanly if the daemon itself uses too much CPU or RAM (for supervisor restart)
- Panic recovery + consecutive error limit + heartbeat log entries

## Install / Run

```bash
# Build
go build -o linux-system-monitor .

# Run (config file must be in the working directory)
./linux-system-monitor
```

Reads temperature from `/sys/class/thermal/thermal_zone*/temp` — no lm-sensors required.

## Key config (linux_system_monitor.yaml)

| Key | Default | Description |
|-----|---------|-------------|
| `cpu_usage_threshold_percent` | 90 | Alert threshold (%) |
| `cpu_temp_threshold_c` | 85 | Alert threshold (°C) |
| `sustain_minutes` | 5 | How long condition must last before alerting |
| `alert_cooldown_minutes` | 30 | Min time between alerts |
| `send_report_cmd` | `["send-report"]` | Command to send alerts |
| `log_file` | `/var/log/linux_system_monitor.log` | Log path |
