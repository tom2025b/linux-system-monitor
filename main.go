// linux-system-monitor: A Go daemon that watches CPU usage and temperature,
// writes alert files to a drop directory when thresholds are sustained,
// and monitors its own resource usage to avoid becoming the problem it watches.
//
// Alert delivery is fully decoupled: this binary writes .txt files to alert_dir;
// entrypoint.sh watches that directory with inotifywait and pipes each new file
// to send-report for email delivery. The daemon never spawns a subprocess, so
// a slow or hung send-report cannot block or crash the monitor.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// gopsutil v3: reads /proc and /sys directly on Linux — no CGO needed.
	// Used for cpu.Percent (aggregate system CPU usage) and process self-monitoring.
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
	"gopkg.in/yaml.v3"
)

// Config maps directly to config.yaml via struct tags.
// All fields have safe defaults applied in loadConfig() for zero values,
// so fields omitted from the YAML file still get sensible values.
type Config struct {
	// System threshold settings
	CPUUsageThreshold    float64 `yaml:"cpu_usage_threshold_percent"`
	CPUTempThresholdC    float64 `yaml:"cpu_temp_threshold_c"`
	SustainMinutes       int     `yaml:"sustain_minutes"`
	CheckIntervalSeconds int     `yaml:"check_interval_seconds"`
	AlertCooldownMinutes int     `yaml:"alert_cooldown_minutes"`

	// Self-safeguard: exit cleanly if this daemon uses too many resources.
	SelfMaxCPUPercent    float64 `yaml:"self_max_cpu_percent"`
	SelfMaxMemMB         float64 `yaml:"self_max_mem_mb"`
	SelfSustainSeconds   int     `yaml:"self_sustain_seconds"`
	MaxConsecutiveErrors int     `yaml:"max_consecutive_errors"`

	// Output paths — must match the container's --volume mounts.
	AlertDir string `yaml:"alert_dir"`
	LogFile  string `yaml:"log_file"`
}

// Package-level state. All mutations happen inside the single ticker goroutine
// (except cancel() in the signal handler), so no mutex is required.
var (
	cfg               Config
	highStart         time.Time        // when the system first crossed a threshold
	lastAlert         time.Time        // when the last alert file was written
	selfHighStart     time.Time        // when the daemon first exceeded its own limits
	consecutiveErrors int              // count of consecutive failed metric reads
	logger            *log.Logger      // file logger, initialised in main()
	ownPID            int              // this process's PID, used for self-monitoring
	selfProc          *process.Process // persistent handle — keeps CPU delta accurate
	thermalWarnOnce   sync.Once        // ensures the "no thermal zones" warning logs once
)

// ── Config loading ────────────────────────────────────────────────────────────

// configPath resolves where to read the config file, in priority order:
//
//  1. $MONITOR_CONFIG environment variable — explicit override for any environment
//  2. /config/config.yaml                  — standard container bind-mount point
//  3. ./config.yaml                        — fallback for local development
func configPath() string {
	// Check the environment variable first so the operator can override without
	// rebuilding or re-mounting the container.
	if p := os.Getenv("MONITOR_CONFIG"); p != "" {
		return p
	}
	// Check the canonical container path. os.Stat is cheap and avoids the
	// confusing error "could not read ./config.yaml" when running in a container.
	if _, err := os.Stat("/config/config.yaml"); err == nil {
		return "/config/config.yaml"
	}
	return "./config.yaml"
}

// loadConfig reads the YAML config file and applies defaults for zero-valued fields.
// Uses stdlib log.Fatalf (not the file logger) because the file logger is not
// open yet when this function runs.
func loadConfig() {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Could not read config from %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Invalid YAML in %s: %v", path, err)
	}

	// Apply safe defaults for any fields left at Go zero-values (0 / "").
	// A zero value means the field was absent from the YAML, not explicitly set to 0.
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
	if cfg.AlertDir == "" {
		cfg.AlertDir = "/alerts"
	}
	if cfg.LogFile == "" {
		cfg.LogFile = "/logs/monitor.log"
	}
}

// validateConfig checks all threshold values are in sensible ranges.
// It collects every error before calling log.Fatal so the user sees them all at once.
func validateConfig() {
	var errs []string

	// CPU usage is a percentage — must be 1–100.
	if cfg.CPUUsageThreshold <= 0 || cfg.CPUUsageThreshold > 100 {
		errs = append(errs, "cpu_usage_threshold_percent must be between 1 and 100")
	}
	// 150°C is well above any real CPU's thermal limit — a safe upper bound.
	if cfg.CPUTempThresholdC <= 0 || cfg.CPUTempThresholdC > 150 {
		errs = append(errs, "cpu_temp_threshold_c must be between 1 and 150")
	}
	// A zero or negative sustain window would fire alerts instantly on any spike.
	if cfg.SustainMinutes <= 0 {
		errs = append(errs, "sustain_minutes must be > 0")
	}
	// A zero interval would spin the CPU reading metrics non-stop.
	if cfg.CheckIntervalSeconds <= 0 {
		errs = append(errs, "check_interval_seconds must be > 0")
	}
	// Cooldown of 0 is allowed (no suppression), but negative makes no sense.
	if cfg.AlertCooldownMinutes < 0 {
		errs = append(errs, "alert_cooldown_minutes must be >= 0")
	}
	if cfg.SelfMaxCPUPercent <= 0 {
		errs = append(errs, "self_max_cpu_percent must be > 0")
	}
	if cfg.SelfMaxMemMB <= 0 {
		errs = append(errs, "self_max_mem_mb must be > 0")
	}
	if cfg.SelfSustainSeconds <= 0 {
		errs = append(errs, "self_sustain_seconds must be > 0")
	}
	if cfg.MaxConsecutiveErrors <= 0 {
		errs = append(errs, "max_consecutive_errors must be > 0")
	}
	if cfg.AlertDir == "" {
		errs = append(errs, "alert_dir must not be empty")
	}

	if len(errs) > 0 {
		for _, e := range errs {
			log.Printf("Config error: %s", e)
		}
		log.Fatal("Fix the config errors above and restart")
	}
}

// checkLogDir ensures the log file's parent directory exists and is writable.
// Called before opening the log file so failures produce a clear message
// rather than a cryptic "permission denied" mid-run.
func checkLogDir() {
	dir := filepath.Dir(cfg.LogFile)

	// MkdirAll creates the directory (and any parents) if it doesn't exist.
	// 0755 = owner rwx, group+other rx — standard for system directories.
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatalf("Cannot create log directory %s: %v", dir, err)
	}

	// Probe writability by creating and immediately removing a temp file.
	// This catches permission issues before we commit to running as a daemon.
	probe := filepath.Join(dir, ".write_probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Log directory %s is not writable: %v", dir, err)
	}
	f.Close()
	os.Remove(probe) // best-effort cleanup; ignore error
}

// ── Temperature reading ───────────────────────────────────────────────────────

// isCPUThermalZone returns true if the zone at tempPath is a CPU sensor.
// It reads the "type" file that is a sibling of "temp" in the zone directory
// and matches it against known CPU thermal zone type identifiers.
//
// If the type file is missing or unreadable, returns true conservatively —
// better to include an unknown zone than to silently miss the CPU sensor.
func isCPUThermalZone(tempPath string) bool {
	// The zone directory holds both "temp" and "type".
	// e.g. /sys/class/thermal/thermal_zone0/temp → /sys/class/thermal/thermal_zone0/type
	typeFile := filepath.Join(filepath.Dir(tempPath), "type")
	data, err := os.ReadFile(typeFile)
	if err != nil {
		// Can't determine zone type — include it conservatively.
		return true
	}

	zoneType := strings.TrimSpace(string(data))

	// Known CPU thermal zone type identifiers across common architectures:
	//   x86_pkg_temp — Intel CPU die temperature (most x86 systems)
	//   cpu-thermal   — ARM / Raspberry Pi CPU
	//   cpu0-thermal  — Some ARM SoCs
	//   soc-thermal   — Generic SoC thermal zone (usually the CPU)
	//   acpitz        — ACPI thermal zone (typically CPU on desktops and laptops)
	for _, t := range []string{"x86_pkg_temp", "cpu-thermal", "cpu0-thermal", "soc-thermal", "acpitz"} {
		if zoneType == t {
			return true
		}
	}
	return false
}

// readMaxThermalTemp scans thermal zones under baseDir and returns the highest
// CPU zone temperature in degrees Celsius.
//
// baseDir in production: "/sys/class/thermal"
// baseDir in tests:      a temp directory that mirrors the /sys structure
//
// Non-CPU zones (battery, NVMe, PCH, etc.) are filtered out by isCPUThermalZone.
// Returns 0 if no CPU zones are found — 0 < any valid threshold so no false alerts.
func readMaxThermalTemp(baseDir string) float64 {
	// Glob expands to every thermal_zone*/temp path under baseDir.
	zones, err := filepath.Glob(filepath.Join(baseDir, "thermal_zone*/temp"))
	if err != nil || len(zones) == 0 {
		// sync.Once ensures this warning appears exactly once per process run,
		// not on every tick — avoids log spam on VMs with no thermal sensors.
		thermalWarnOnce.Do(func() {
			logger.Println("No thermal zones found — temperature monitoring disabled (normal inside VMs)")
		})
		return 0
	}

	maxTemp := 0.0
	for _, path := range zones {
		// Skip zones that are not CPU sensors.
		if !isCPUThermalZone(path) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue // zone may be offline or permission-denied; skip silently
		}

		// TrimSpace removes the trailing newline that the kernel always appends.
		// The kernel reports temperature in millidegrees Celsius (48000 = 48°C).
		milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}

		if tempC := milliC / 1000.0; tempC > maxTemp {
			maxTemp = tempC
		}
	}
	return maxTemp
}

// ── Alert writing ─────────────────────────────────────────────────────────────

// alertTriggers returns a slug encoding which threshold(s) fired.
// Used in the alert filename and email subject so the recipient immediately
// knows why the alert fired without opening the body.
//
// Possible values: "cpu", "temp", "cpu+temp", "unknown"
// "unknown" should never occur in practice — isHigh must be true when called.
func alertTriggers(cpuUsage, cpuTempC float64) string {
	var parts []string
	if cpuUsage >= cfg.CPUUsageThreshold {
		parts = append(parts, "cpu")
	}
	if cpuTempC >= cfg.CPUTempThresholdC {
		parts = append(parts, "temp")
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "+")
}

// writeAlert writes an alert .txt file to cfg.AlertDir.
// Filename format: YYYY-MM-DD_HH-MM-SS_<trigger>.txt
// (colons omitted from the time portion to avoid issues on certain filesystems)
//
// entrypoint.sh watches cfg.AlertDir with inotifywait. When a new file appears,
// the watcher pipes it to send-report for email delivery. This means the daemon
// never spawns a subprocess — a hung send-report cannot block the monitor.
//
// lastAlert is set on every successful write so the cooldown logic is reliable
// regardless of whether send-report later succeeds or fails.
func writeAlert(cpuUsage, cpuTempC float64) error {
	now := time.Now()
	trigger := alertTriggers(cpuUsage, cpuTempC)

	filename := fmt.Sprintf("%s_%s.txt", now.Format("2006-01-02_15-04-05"), trigger)
	path := filepath.Join(cfg.AlertDir, filename)

	// Build the list of human-readable reasons to include in the alert body.
	var reasons []string
	if cpuUsage >= cfg.CPUUsageThreshold {
		reasons = append(reasons, fmt.Sprintf(
			"  • CPU usage %.1f%% exceeded threshold %.0f%%",
			cpuUsage, cfg.CPUUsageThreshold,
		))
	}
	if cpuTempC >= cfg.CPUTempThresholdC {
		reasons = append(reasons, fmt.Sprintf(
			"  • CPU temp %.1f°C exceeded threshold %.0f°C",
			cpuTempC, cfg.CPUTempThresholdC,
		))
	}

	body := fmt.Sprintf(`System Health Alert — %s

Triggered by:
%s

Current readings:
  CPU Usage:  %.1f%%
  CPU Temp:   %.1f°C

Condition sustained for over %d minute(s).
Check cooling and workload now.

Log: %s
`,
		now.Format("2006-01-02 15:04:05"),
		strings.Join(reasons, "\n"),
		cpuUsage,
		cpuTempC,
		cfg.SustainMinutes,
		cfg.LogFile,
	)

	// os.WriteFile creates or truncates the file atomically enough for our
	// purposes. inotifywait fires on close_write — after the file is fully
	// written and the file descriptor is closed.
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return fmt.Errorf("write alert file %s: %w", path, err)
	}

	// Set lastAlert so the cooldown suppresses the next alert correctly.
	lastAlert = now
	logger.Printf("Alert written: %s", filename)
	return nil
}

// ── Metrics ───────────────────────────────────────────────────────────────────

// getMetrics samples CPU usage (averaged over 500ms) and max CPU temperature.
// Returns an error only if cpu.Percent fails; temperature errors are non-fatal
// (temp stays 0, which is below any valid threshold — no false alerts).
func getMetrics() (cpuUsage, cpuTempC float64, err error) {
	// cpu.Percent with a 500ms interval blocks briefly, then returns a single
	// aggregate usage value across all cores (false = don't split per-core).
	usage, err := cpu.Percent(500*time.Millisecond, false)
	if err != nil {
		return 0, 0, err
	}
	if len(usage) > 0 {
		cpuUsage = usage[0]
	}

	// Read temperature from the real kernel path in production.
	// Tests call readMaxThermalTemp(tempDir) directly to use fake data.
	cpuTempC = readMaxThermalTemp("/sys/class/thermal")
	return cpuUsage, cpuTempC, nil
}

// checkSelfResources returns this daemon's CPU% and RSS memory in MB.
// Uses the persistent selfProc handle so CPUPercent() measures the delta
// since the previous call — accurate without a blocking sleep each tick.
func checkSelfResources() (cpuPct, memMB float64, err error) {
	// CPUPercent() on a persistent handle measures CPU ticks elapsed since
	// the last call. The first call (at startup) primes the baseline.
	cpuPct, err = selfProc.CPUPercent()
	if err != nil {
		return 0, 0, err
	}

	// MemoryInfo returns RSS (resident set size) — actual RAM pages in use,
	// not virtual memory. Divide by 1024*1024 to convert bytes → megabytes.
	mem, err := selfProc.MemoryInfo()
	if err != nil {
		return 0, 0, err
	}
	memMB = float64(mem.RSS) / 1024 / 1024
	return cpuPct, memMB, nil
}

// ── Entry point ───────────────────────────────────────────────────────────────

func main() {
	loadConfig()
	validateConfig()

	ownPID = os.Getpid()
	checkLogDir()

	// Open (or create) the log file in append mode so history survives restarts.
	// 0644 = owner rw, group+other r.
	logFile, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Cannot open log file: %v", err)
	}
	defer logFile.Close()

	// log.New writes to logFile with a "MONITOR: " prefix plus timestamp and file info.
	logger = log.New(logFile, "MONITOR: ", log.LstdFlags|log.Lshortfile)
	logger.Printf("Linux System Monitor started — PID %d", ownPID)

	// Build a persistent process handle for self-monitoring.
	// Keeping one handle across ticks lets CPUPercent() measure deltas correctly.
	selfProc, err = process.NewProcess(int32(ownPID))
	if err != nil {
		log.Fatalf("Cannot get self process handle: %v", err)
	}
	// Prime the CPU baseline. The first call always returns 0; every subsequent
	// call returns real CPU% since this call. Discard the priming result.
	_, _ = selfProc.CPUPercent()

	// Deferred panic recovery: if anything unexpected panics, log it with a full
	// stack trace before exiting. Note: log.Fatal calls os.Exit(1) directly —
	// deferred functions do NOT run for Fatal. Only real panics reach this recover.
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC recovered: %v\nStack:\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	// context.WithCancel gives a clean way to stop the ticker loop on signal.
	// cancel() closes ctx.Done(), unblocking the select below.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for SIGINT (Ctrl+C) and SIGTERM (podman stop / systemd stop).
	// The goroutine calls cancel() which signals the main loop to exit.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Println("Shutting down gracefully")
		cancel()
	}()

	// time.NewTicker fires at fixed wall-clock intervals regardless of how long
	// the loop body takes — more accurate than sleep loops for daemons.
	ticker := time.NewTicker(time.Duration(cfg.CheckIntervalSeconds) * time.Second)
	defer ticker.Stop()

	heartbeat := 0

	for {
		select {
		case <-ctx.Done():
			// Signal received — exit the loop; defers clean up.
			return

		case <-ticker.C:
			heartbeat++
			// Log a heartbeat every 10 ticks so you can confirm the daemon is
			// alive even during long periods with nothing to report.
			if heartbeat%10 == 0 {
				logger.Println("Heartbeat — monitor still alive")
			}

			// ── 1. SYSTEM METRICS ─────────────────────────────────────────
			cpuUsage, cpuTempC, metricErr := getMetrics()
			if metricErr != nil {
				consecutiveErrors++
				logger.Printf("Metric error (%d/%d): %v",
					consecutiveErrors, cfg.MaxConsecutiveErrors, metricErr)
				// Too many consecutive failures likely means a kernel/driver issue.
				// Exit so the supervisor can restart us with a clean state.
				if consecutiveErrors >= cfg.MaxConsecutiveErrors {
					logger.Fatal("Too many consecutive metric errors — exiting for restart")
				}
				continue // skip threshold checks for this tick
			}
			consecutiveErrors = 0 // reset on any successful read

			logger.Printf("Check → CPU %.1f%% | Temp %.1f°C", cpuUsage, cpuTempC)

			// isHigh fires when EITHER threshold is exceeded (OR logic).
			// cpuTempC is 0 when no thermal zones exist (VMs) — 0 < any valid
			// threshold, so missing sensors never cause false alerts.
			isHigh := (cpuTempC >= cfg.CPUTempThresholdC) || (cpuUsage >= cfg.CPUUsageThreshold)
			now := time.Now()

			if isHigh {
				if highStart.IsZero() {
					// First tick above threshold — record when the condition started.
					highStart = now
				}
				sustained := time.Since(highStart) >= time.Duration(cfg.SustainMinutes)*time.Minute
				cooledDown := time.Since(lastAlert) >= time.Duration(cfg.AlertCooldownMinutes)*time.Minute
				if sustained && cooledDown {
					if err := writeAlert(cpuUsage, cpuTempC); err != nil {
						logger.Printf("Failed to write alert file: %v", err)
					}
					// Reset so the next distinct sustained period can trigger again.
					highStart = time.Time{}
				}
			} else {
				// Condition cleared — reset the sustain timer.
				highStart = time.Time{}
			}

			// ── 2. SELF-RESOURCE SAFEGUARD ────────────────────────────────
			selfCPU, selfMem, selfErr := checkSelfResources()
			if selfErr != nil {
				// Bug fix: reset selfHighStart so a stale timer from a previous
				// high period doesn't linger when self-checks start failing.
				selfHighStart = time.Time{}
				logger.Printf("Self-resource check failed (non-fatal): %v", selfErr)
				continue
			}

			selfOver := (selfCPU >= cfg.SelfMaxCPUPercent) || (selfMem >= cfg.SelfMaxMemMB)
			if selfOver {
				if selfHighStart.IsZero() {
					selfHighStart = now
				}
				// Exit cleanly if we've been over our own limits too long.
				// Podman's restart policy (--restart=unless-stopped) brings us back.
				if time.Since(selfHighStart) >= time.Duration(cfg.SelfSustainSeconds)*time.Second {
					logger.Fatalf("SELF OVERLOAD: CPU %.1f%% / Mem %.1fMB — exiting for restart",
						selfCPU, selfMem)
				}
			} else {
				selfHighStart = time.Time{}
			}
		}
	}
}

// ── Learning Notes ────────────────────────────────────────────────────────────
// • Drop-directory alerting: the daemon writes files; a watcher delivers them.
//   This fully decouples monitoring from delivery — a hung send-report cannot
//   block or crash the monitor.
// • context.WithCancel: the idiomatic Go way to propagate shutdown. cancel()
//   closes ctx.Done(), unblocking any select that watches it.
// • time.NewTicker vs sleep: Ticker fires at fixed wall-clock intervals
//   regardless of loop body duration — essential for consistent daemon timing.
// • Persistent process.Process handle: keeping one handle lets CPUPercent()
//   measure a real CPU-time delta between calls without a blocking sleep.
// • sync.Once: runs a function exactly once across all goroutines. Used here
//   to log the "no thermal zones" warning once, not on every tick.
// • log.Fatal calls os.Exit(1) — deferred functions do NOT run. Only real
//   panics reach the defer recover() block.
// • selfHighStart reset on error: clears the stale timer so repeated self-check
//   failures don't silently accumulate toward a false overload exit.
