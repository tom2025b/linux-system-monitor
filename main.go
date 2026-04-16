// linux-system-monitor: A Go daemon that watches CPU usage and temperature,
// sends alerts via send-report when thresholds are sustained, and monitors
// its own resource usage so it can't become the problem it's watching for.

package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	// gopsutil v3: cross-platform system stats for Go
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
	"gopkg.in/yaml.v3"
)

// Config maps directly to linux_system_monitor.yaml via struct tags.
// yaml.Unmarshal fills each field by matching the tag name to the YAML key.
type Config struct {
	CPUUsageThreshold    float64 `yaml:"cpu_usage_threshold_percent"`
	CPUTempThresholdC    float64 `yaml:"cpu_temp_threshold_c"`
	SustainMinutes       int     `yaml:"sustain_minutes"`
	CheckIntervalSeconds int     `yaml:"check_interval_seconds"`
	AlertCooldownMinutes int     `yaml:"alert_cooldown_minutes"`

	// Self-safeguards: if this daemon itself gets too hungry, it exits cleanly
	// so systemd (or whatever supervisor) can restart it fresh.
	SelfMaxCPUPercent    float64 `yaml:"self_max_cpu_percent"`
	SelfMaxMemMB         float64 `yaml:"self_max_mem_mb"`
	SelfSustainSeconds   int     `yaml:"self_sustain_seconds"`
	MaxConsecutiveErrors int     `yaml:"max_consecutive_errors"`

	// SendReportCmd is the command + args used to send alerts.
	// The alert body is piped to stdin; subject is appended as a trailing arg.
	SendReportCmd []string `yaml:"send_report_cmd"`
	LogFile       string   `yaml:"log_file"`
}

// Package-level state — these persist across ticker ticks so we can track
// how long a condition has been sustained.
var (
	cfg               Config
	highStart         time.Time // when system first went over threshold
	lastAlert         time.Time // when we last sent an alert (for cooldown)
	selfHighStart     time.Time // when daemon itself first went over its own limits
	consecutiveErrors int       // how many metric reads have failed in a row
	logger            *log.Logger
	ownPID            int // this process's PID, used for self-monitoring
)

// loadConfig reads and parses linux_system_monitor.yaml, applying safe defaults
// for optional self-safeguard fields if they're missing from the file.
func loadConfig() {
	data, err := os.ReadFile("linux_system_monitor.yaml")
	if err != nil {
		log.Fatalf("❌ Could not read config: %v", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("❌ Invalid YAML: %v", err)
	}
	if len(cfg.SendReportCmd) == 0 {
		log.Fatal("❌ send_report_cmd must be set in YAML")
	}

	// Apply defaults for fields that weren't set in YAML.
	// Go zero-values (0 / 0.0) mean "not configured", so we fill them in.
	if cfg.SelfMaxCPUPercent == 0 {
		cfg.SelfMaxCPUPercent = 20
	}
	if cfg.SelfMaxMemMB == 0 {
		cfg.SelfMaxMemMB = 100
	}
	if cfg.SelfSustainSeconds == 0 {
		cfg.SelfSustainSeconds = 120
	}
	if cfg.MaxConsecutiveErrors == 0 {
		cfg.MaxConsecutiveErrors = 5
	}
}

// getMetrics samples CPU usage (averaged over 500ms) and the highest CPU-related
// temperature from lm-sensors. Returns an error only if cpu.Percent fails;
// temperature errors are non-fatal (temp just stays 0).
func getMetrics() (cpuUsage, cpuTempC float64, err error) {
	// cpu.Percent with a 500ms interval blocks briefly then returns a single
	// aggregate usage value across all cores (false = don't split per-core).
	usage, err := cpu.Percent(500*time.Millisecond, false)
	if err != nil {
		return 0, 0, err
	}
	if len(usage) > 0 {
		cpuUsage = usage[0]
	}

	// Read CPU temperature directly from /sys/class/thermal/thermal_zone*/temp.
	// This is the standard Linux kernel interface — no lm-sensors needed.
	// Each file contains the temperature in millidegrees Celsius (e.g. 45000 = 45°C).
	cpuTempC = readMaxThermalTemp()

	return cpuUsage, cpuTempC, nil
}

// checkSelfResources returns this daemon's own CPU% and RSS memory usage.
// Uses process.CPUPercent with a 500ms sample window for an accurate reading.
func checkSelfResources() (cpuPct, memMB float64, err error) {
	// NewProcess looks up our PID in /proc to get a handle on ourselves.
	proc, err := process.NewProcess(int32(ownPID))
	if err != nil {
		return 0, 0, err
	}

	// CPUPercent in gopsutil v3 takes no interval argument — it computes CPU%
	// since the last call for this process handle (or since process start on
	// the first call). Call it twice with a short sleep for a fresh sample.
	_, _ = proc.CPUPercent() // prime the baseline measurement
	time.Sleep(500 * time.Millisecond)
	cpuPct, err = proc.CPUPercent()
	if err != nil {
		return 0, 0, err
	}

	// MemoryInfo returns RSS (resident set size) — actual RAM in use, not virtual.
	mem, err := proc.MemoryInfo()
	if err != nil {
		return 0, 0, err
	}
	// Convert bytes → megabytes for comparison against config threshold.
	memMB = float64(mem.RSS) / 1024 / 1024
	return cpuPct, memMB, nil
}

// readMaxThermalTemp scans /sys/class/thermal/thermal_zone*/temp and returns
// the highest temperature found, in degrees Celsius.
// Each file holds an integer in millidegrees (e.g. "48000" = 48°C).
// Returns 0 if no thermal zones are readable (e.g. inside a VM).
func readMaxThermalTemp() float64 {
	// Glob expands the wildcard to every thermal zone the kernel exposes.
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(zones) == 0 {
		return 0
	}
	maxTemp := 0.0
	for _, path := range zones {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // this zone may be offline or permission-denied
		}
		// TrimSpace removes any trailing newline before parsing.
		milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		// Kernel reports in millidegrees — divide by 1000 for Celsius.
		tempC := milliC / 1000.0
		if tempC > maxTemp {
			maxTemp = tempC
		}
	}
	return maxTemp
}

// sendAlert fires an email via send-report with CPU usage and temp details.
// It records lastAlert so the cooldown logic can suppress repeat alerts.
func sendAlert(cpuUsage, cpuTempC float64) {
	now := time.Now()
	subject := fmt.Sprintf("🚨 Linux Alert: System stress! CPU %.0f%% @ %.1f°C", cpuUsage, cpuTempC)

	// The body is piped to stdin of send-report.
	body := fmt.Sprintf(`System Health Report - %s

CPU Usage:  %.1f%%
CPU Temp:   %.1f°C

Condition sustained for over %d minute(s).
Check cooling / workload now.

Log: %s
`, now.Format("2006-01-02 15:04:05"), cpuUsage, cpuTempC, cfg.SustainMinutes, cfg.LogFile)

	// Append subject as a trailing CLI argument (harmless if send-report ignores it).
	cmdArgs := append(cfg.SendReportCmd, subject)
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin = bytes.NewBufferString(body)

	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Printf("❌ send-report failed: %v\nOutput: %s", err, output)
	} else {
		logger.Println("✅ Alert sent via send-report")
		lastAlert = now
	}
}

func main() {
	loadConfig()
	ownPID = os.Getpid()

	// Open (or create) the log file in append mode so history is preserved
	// across restarts. 0644 = owner rw, group+other r.
	logFile, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Cannot open log file: %v", err)
	}
	defer logFile.Close()

	// log.New writes to logFile with a "MONITOR: " prefix and timestamp+file info.
	logger = log.New(logFile, "MONITOR: ", log.LstdFlags|log.Lshortfile)
	logger.Printf("🚀 Linux System Monitor started — PID %d", ownPID)

	// Deferred panic recovery: if anything unexpected panics, log it with a
	// full stack trace before exiting. This prevents silent crashes.
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("💥 PANIC recovered: %v\nStack: %s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	// context.WithCancel gives us a clean way to stop all goroutines when
	// a signal arrives. cancel() triggers ctx.Done() everywhere.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for SIGINT (Ctrl+C) and SIGTERM (systemd stop / kill).
	// The goroutine calls cancel() which unblocks the main select loop.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Println("👋 Shutting down gracefully")
		cancel()
	}()

	// time.NewTicker fires on a regular interval. We drive all work from it
	// rather than sleep loops, so timing stays consistent.
	ticker := time.NewTicker(time.Duration(cfg.CheckIntervalSeconds) * time.Second)
	defer ticker.Stop()

	heartbeat := 0 // counts ticks; used to emit a periodic "still alive" log line

	for {
		select {
		case <-ctx.Done():
			// Signal received — exit the loop and let defers clean up.
			return

		case <-ticker.C:
			heartbeat++
			// Every 10 ticks, log a heartbeat so you can tell the daemon is alive
			// even when nothing is wrong.
			if heartbeat%10 == 0 {
				logger.Println("❤️  Heartbeat — monitor still alive")
			}

			// ── 1. SYSTEM METRICS ───────────────────────────────────────────
			cpuUsage, cpuTempC, metricErr := getMetrics()
			if metricErr != nil {
				consecutiveErrors++
				logger.Printf("⚠️  Metric error (%d/%d): %v",
					consecutiveErrors, cfg.MaxConsecutiveErrors, metricErr)
				// Too many consecutive failures likely means a driver issue.
				// Exit so the supervisor can restart us rather than spinning forever.
				if consecutiveErrors >= cfg.MaxConsecutiveErrors {
					logger.Fatal("❌ Too many consecutive errors — exiting for restart")
				}
				continue // skip the rest of this tick
			}
			consecutiveErrors = 0 // reset on any successful read

			logger.Printf("Check → CPU %.1f%% | Temp %.1f°C", cpuUsage, cpuTempC)

			// isHigh is true if either threshold is exceeded.
			isHigh := (cpuTempC >= cfg.CPUTempThresholdC) || (cpuUsage >= cfg.CPUUsageThreshold)
			now := time.Now()

			if isHigh {
				if highStart.IsZero() {
					// First tick we've seen the condition — record when it started.
					highStart = now
				}
				// Only alert if sustained long enough AND cooldown has passed.
				sustained := time.Since(highStart) >= time.Duration(cfg.SustainMinutes)*time.Minute
				cooledDown := time.Since(lastAlert) >= time.Duration(cfg.AlertCooldownMinutes)*time.Minute
				if sustained && cooledDown {
					sendAlert(cpuUsage, cpuTempC)
					highStart = time.Time{} // reset so the next sustained period can trigger again
				}
			} else {
				// Condition cleared — reset the sustain timer.
				highStart = time.Time{}
			}

			// ── 2. SELF-RESOURCE SAFEGUARD ──────────────────────────────────
			selfCPU, selfMem, selfErr := checkSelfResources()
			if selfErr != nil {
				// Non-fatal: system monitoring still works, just log and continue.
				logger.Printf("⚠️  Self-resource check failed (non-fatal): %v", selfErr)
				continue
			}

			selfOver := (selfCPU >= cfg.SelfMaxCPUPercent) || (selfMem >= cfg.SelfMaxMemMB)
			if selfOver {
				if selfHighStart.IsZero() {
					selfHighStart = now
				}
				// If we've been over our own limits for too long, exit cleanly.
				// systemd / supervisord will restart us with a clean slate.
				if time.Since(selfHighStart) >= time.Duration(cfg.SelfSustainSeconds)*time.Second {
					logger.Fatalf("❌ SELF OVERLOAD: CPU %.1f%% / Mem %.1fMB — exiting for restart",
						selfCPU, selfMem)
				}
			} else {
				selfHighStart = time.Time{}
			}
		}
	}
}

// ── Learning Notes ───────────────────────────────────────────────────────────
// • gopsutil v3: Go library for cross-platform system stats (/proc on Linux).
//   Sensors package only supports Temperatures(); fan speed requires custom
//   lm-sensors parsing if needed.
// • context.WithCancel: the idiomatic way to propagate shutdown signals in Go.
//   cancel() closes the ctx.Done() channel, unblocking any select that watches it.
// • time.NewTicker vs time.Sleep: Ticker fires at fixed wall-clock intervals
//   regardless of how long the body takes — more accurate for daemons.
// • log.Fatal calls os.Exit(1) after logging — deferred functions do NOT run.
//   Use it only for truly unrecoverable errors where cleanup doesn't matter.
// • defer recover(): the only way to catch a panic in Go. Must be in a deferred
//   function or it won't execute after the panic unwinds the stack.
