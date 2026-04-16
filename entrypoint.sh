#!/bin/sh
# entrypoint.sh — PID 1 for the linux-system-monitor container.
#
# Responsibilities:
#   1. Ensure /alerts and /logs directories exist inside the container.
#   2. Start an inotifywait watcher in the background. When the Go monitor
#      writes an alert file to /alerts/, the watcher pipes it to send-report.
#   3. Replace this shell with the Go binary via exec, so the binary becomes
#      PID 1 and receives SIGTERM directly from 'podman stop'.
#
# Alert filenames follow the pattern: YYYY-MM-DD_HH-MM-SS_<trigger>.txt
# where <trigger> is one of: cpu, temp, cpu+temp

# set -e exits immediately if any setup command fails.
# Applied only to the setup section; the watcher pipeline runs independently.
set -e

# Ensure runtime directories exist. These may already be created by the image
# layer or shadowed by bind mounts, but mkdir -p is idempotent and harmless.
mkdir -p /alerts /logs

# ── Start the inotify watcher (background) ───────────────────────────────────
# inotifywait -m: monitor continuously — don't exit after the first event.
# -e close_write: fire only when a file is fully written and closed. This
#   avoids reading a partial file on the open or mid-write events.
# --format '%f': print only the bare filename (not the full path or event name).
# 2>/dev/null: suppress inotifywait's startup banner from container logs.
#
# The entire pipeline (inotifywait + while loop) is backgrounded with & so
# we can exec the Go binary in the foreground below.
inotifywait -m -e close_write --format '%f' /alerts/ 2>/dev/null |
while IFS= read -r filename; do
    # Derive the email subject from the trigger suffix in the filename.
    # Filename pattern: 2026-04-16_14-32-00_cpu+temp.txt
    # sed strips everything up to and including the last underscore,
    # then strips the .txt extension, leaving just the trigger slug.
    trigger=$(echo "$filename" | sed 's/.*_\(.*\)\.txt$/\1/')
    subject="Linux Monitor Alert: $trigger threshold exceeded"

    # Pipe the alert file body to send-report with the subject as $1.
    # Running in a subshell (&) means a slow or hung send-report cannot
    # block the watcher loop from processing the next alert file.
    send-report "$subject" < "/alerts/$filename" &
done &
# The trailing & backgrounds the entire inotifywait pipeline.

# ── Replace shell with Go binary (foreground) ────────────────────────────────
# exec replaces this shell process with the Go binary.
# Effects:
#   • The Go binary becomes PID 1 in the container.
#   • 'podman stop' sends SIGTERM directly to the binary (not through a shell).
#   • The existing SIGTERM handler in main.go shuts the daemon down cleanly.
exec /usr/local/bin/linux-system-monitor
