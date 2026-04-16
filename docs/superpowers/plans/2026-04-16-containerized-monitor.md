# Containerized Linux System Monitor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite linux-system-monitor as a Podman container that fixes all 7 review bugs and delivers alerts via a drop-directory watched by an inotify loop inside the container.

**Architecture:** The Go binary monitors CPU usage and temperature and writes alert `.txt` files to `/alerts/` — it never spawns a subprocess. An `entrypoint.sh` watcher picks up new files with `inotifywait` and pipes them to `send-report`. A multi-stage Containerfile produces a minimal Alpine image with `msmtp` and `inotify-tools`.

**Tech Stack:** Go 1.26 · gopsutil v3 · Alpine 3.19 · inotify-tools · msmtp · Podman

**Spec:** `docs/superpowers/specs/2026-04-16-containerized-monitor-design.md`

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `config.yaml` | Create (replaces `linux_system_monitor.yaml`) | All runtime settings |
| `main.go` | Rewrite | Go daemon: metrics, threshold tracking, alert file writing |
| `main_test.go` | Create | Unit tests for pure/file-based functions |
| `entrypoint.sh` | Create | PID 1: inotify watcher (bg) + exec Go binary (fg) |
| `Containerfile` | Create | Builder → debug target → final Alpine image |
| `README.md` | Rewrite | Container-first quickstart |
| `linux_system_monitor.yaml` | Delete | Replaced by `config.yaml` |
| `linux-system-monitor` (binary) | Add to `.gitignore` | Build artifact, not committed |

---

## Task 1: Cleanup and new config.yaml

**Files:**
- Delete: `linux_system_monitor.yaml`
- Create: `config.yaml`
- Modify: `.gitignore` (create if absent)

- [ ] **Step 1: Remove old config and compiled binary from repo**

```bash
cd ~/projects/linux-system-monitor
rm -f linux_system_monitor.yaml linux-system-monitor
echo "linux-system-monitor" >> .gitignore
```

- [ ] **Step 2: Create config.yaml**

```yaml
# config.yaml — linux-system-monitor runtime configuration.
# Mount read-only into the container at /config/config.yaml.
#
# Config path resolution order (first match wins):
#   1. $MONITOR_CONFIG environment variable
#   2. /config/config.yaml  (default container mount point)
#   3. ./config.yaml        (fallback for local development)

# ── System Thresholds ────────────────────────────────────────────────────────

# Alert if CPU usage stays above this percentage for sustain_minutes.
cpu_usage_threshold_percent: 90

# Alert if CPU temperature stays above this Celsius value for sustain_minutes.
# Only real CPU thermal zones are read (x86_pkg_temp, cpu-thermal, etc.).
# Battery, NVMe, PCH, and other non-CPU zones are filtered out.
cpu_temp_threshold_c: 85

# Both thresholds use OR logic: either one sustained triggers an alert.

# How many consecutive minutes the high condition must last before alerting.
sustain_minutes: 5

# How often to poll the system, in seconds.
check_interval_seconds: 60

# Minimum minutes between alerts. Prevents spam during sustained high load.
# Set to 0 to disable cooldown (not recommended).
alert_cooldown_minutes: 30

# ── Self-Safeguards ──────────────────────────────────────────────────────────
# If the daemon itself exceeds these resource limits continuously for
# self_sustain_seconds, it exits cleanly so Podman's restart policy can
# bring it back with a fresh process.

self_max_cpu_percent: 20
self_max_mem_mb: 100
self_sustain_seconds: 120
max_consecutive_errors: 5

# ── Paths (match your --volume mounts) ──────────────────────────────────────

# Directory where the daemon writes alert .txt files.
# entrypoint.sh watches this dir with inotifywait and delivers via send-report.
alert_dir: /alerts

# Log file path inside the container.
log_file: /logs/monitor.log
```

- [ ] **Step 3: Commit**

```bash
cd ~/projects/linux-system-monitor
git add config.yaml .gitignore
git rm --cached linux_system_monitor.yaml linux-system-monitor 2>/dev/null || true
git rm linux_system_monitor.yaml 2>/dev/null || true
git commit -m "replace old yaml config with config.yaml, add .gitignore"
```

---

## Task 2: Scaffold main.go and write all tests

**Files:**
- Rewrite: `main.go` (full scaffold with stubs)
- Create: `main_test.go`

- [ ] **Step 1: Write main.go scaffold**

Replace the entire file with stubs so the test file compiles. Each function body will be filled in by subsequent tasks.

```go
// linux-system-monitor: A Go daemon that watches CPU usage and temperature,
// writes alert files to a drop directory when thresholds are sustained,
// and monitors its own resource usage to avoid becoming the problem it watches.
//
// Alert delivery is decoupled: the daemon writes .txt files to alert_dir;
// entrypoint.sh watches that directory with inotifywait and pipes each file
// to send-report. The daemon never spawns a subprocess.

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
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
	"gopkg.in/yaml.v3"
)

// Config maps directly to config.yaml via struct tags.
// All fields have safe defaults applied in loadConfig() for zero values.
type Config struct {
	CPUUsageThreshold    float64 `yaml:"cpu_usage_threshold_percent"`
	CPUTempThresholdC    float64 `yaml:"cpu_temp_threshold_c"`
	SustainMinutes       int     `yaml:"sustain_minutes"`
	CheckIntervalSeconds int     `yaml:"check_interval_seconds"`
	AlertCooldownMinutes int     `yaml:"alert_cooldown_minutes"`
	SelfMaxCPUPercent    float64 `yaml:"self_max_cpu_percent"`
	SelfMaxMemMB         float64 `yaml:"self_max_mem_mb"`
	SelfSustainSeconds   int     `yaml:"self_sustain_seconds"`
	MaxConsecutiveErrors int     `yaml:"max_consecutive_errors"`
	AlertDir             string  `yaml:"alert_dir"`
	LogFile              string  `yaml:"log_file"`
}

// Package-level state — all mutations happen in the single ticker goroutine,
// so no mutex is needed.
var (
	cfg               Config
	highStart         time.Time        // when system first crossed threshold
	lastAlert         time.Time        // when last alert file was written
	selfHighStart     time.Time        // when daemon first exceeded own limits
	consecutiveErrors int              // consecutive failed metric reads
	logger            *log.Logger
	ownPID            int
	selfProc          *process.Process // persistent handle for self CPU tracking
	thermalWarnOnce   sync.Once        // ensures the "no thermal zones" warning logs once
)

// configPath resolves where to read the config file, checking in priority order.
func configPath() string { return "" }

// loadConfig reads the YAML config file and applies defaults for zero-valued fields.
func loadConfig() {}

// validateConfig checks all threshold values are in sensible ranges.
// Logs every problem found, then calls log.Fatal if any errors exist.
func validateConfig() {}

// checkLogDir ensures the log file's parent directory exists and is writable.
func checkLogDir() {}

// isCPUThermalZone returns true if the thermal zone at tempPath is a CPU sensor.
// It reads the zone's "type" file (sibling of "temp") and matches against known
// CPU thermal zone type identifiers.
func isCPUThermalZone(tempPath string) bool { return false }

// readMaxThermalTemp scans thermal zones under baseDir and returns the highest
// CPU zone temperature in degrees Celsius. Non-CPU zones (battery, NVMe, etc.)
// are ignored. Returns 0 if no CPU zones are found.
func readMaxThermalTemp(baseDir string) float64 { return 0 }

// getMetrics samples CPU usage (500ms average) and max CPU temperature.
func getMetrics() (cpuUsage, cpuTempC float64, err error) { return 0, 0, nil }

// checkSelfResources returns this daemon's CPU% and RSS memory in MB.
func checkSelfResources() (cpuPct, memMB float64, err error) { return 0, 0, nil }

// alertTriggers returns a slug describing which threshold(s) fired.
// e.g. "cpu", "temp", "cpu+temp", or "unknown" if neither (shouldn't happen).
func alertTriggers(cpuUsage, cpuTempC float64) string { return "" }

// writeAlert writes an alert .txt file to cfg.AlertDir and sets lastAlert.
// The filename encodes which threshold(s) triggered and the timestamp.
// Returns an error only if the file cannot be written.
func writeAlert(cpuUsage, cpuTempC float64) error { return nil }

func main() {}
```

- [ ] **Step 2: Create main_test.go with all unit tests**

```go
// main_test.go — unit tests for pure and file-based functions in main.go.
// Integration with real /proc and /sys is not tested here; verify manually
// by running the container and observing podman logs.

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain initialises a discard logger so functions that call logger.Println
// don't panic during tests. Real log output goes to /logs/monitor.log at runtime.
func TestMain(m *testing.M) {
	logger = log.New(io.Discard, "", 0)
	os.Exit(m.Run())
}

// ── configPath ────────────────────────────────────────────────────────────────

func TestConfigPath_EnvVar(t *testing.T) {
	// t.Setenv automatically restores the original value after the test.
	t.Setenv("MONITOR_CONFIG", "/custom/path.yaml")
	if got := configPath(); got != "/custom/path.yaml" {
		t.Errorf("configPath() = %q, want /custom/path.yaml", got)
	}
}

func TestConfigPath_Fallback(t *testing.T) {
	// Ensure env var is not set so we exercise the fallback path.
	os.Unsetenv("MONITOR_CONFIG")
	got := configPath()
	// Depending on whether /config/config.yaml exists on this host, the result
	// will be either that path or ./config.yaml — both are valid fallbacks.
	if got != "/config/config.yaml" && got != "./config.yaml" {
		t.Errorf("configPath() = %q, not a recognised fallback", got)
	}
}

// ── isCPUThermalZone ─────────────────────────────────────────────────────────

func TestIsCPUThermalZone(t *testing.T) {
	// Build a fake /sys/class/thermal/thermal_zone0/ in a temp dir.
	dir := t.TempDir()
	zoneDir := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zoneDir, 0755); err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(zoneDir, "temp")
	typeFile := filepath.Join(zoneDir, "type")

	// isCPUThermalZone reads the "type" file that is a sibling of "temp".
	tests := []struct {
		zoneType string
		want     bool
	}{
		{"x86_pkg_temp", true},   // Intel CPU package temperature
		{"cpu-thermal", true},    // ARM/Raspberry Pi CPU
		{"cpu0-thermal", true},   // Some ARM SoCs
		{"soc-thermal", true},    // SoC thermal zone
		{"acpitz", true},         // ACPI thermal — usually CPU
		{"battery", false},       // Battery sensor — must be ignored
		{"pch_skylake", false},   // Platform Controller Hub — must be ignored
		{"INT3400 Thermal", false}, // Intel thermal management — must be ignored
	}

	for _, tt := range tests {
		t.Run(tt.zoneType, func(t *testing.T) {
			os.WriteFile(typeFile, []byte(tt.zoneType+"\n"), 0644)
			if got := isCPUThermalZone(tempPath); got != tt.want {
				t.Errorf("isCPUThermalZone (type=%q) = %v, want %v", tt.zoneType, got, tt.want)
			}
		})
	}
}

func TestIsCPUThermalZone_MissingTypeFile(t *testing.T) {
	// If the type file is unreadable, the function should conservatively
	// return true (include the zone) rather than silently drop it.
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "temp") // type file does not exist
	if got := isCPUThermalZone(tempPath); !got {
		t.Error("isCPUThermalZone with missing type file should return true (conservative)")
	}
}

// ── readMaxThermalTemp ────────────────────────────────────────────────────────

func TestReadMaxThermalTemp_FiltersNonCPU(t *testing.T) {
	dir := t.TempDir()

	// Zone 0: CPU zone at 48°C — should be included.
	zone0 := filepath.Join(dir, "thermal_zone0")
	os.MkdirAll(zone0, 0755)
	os.WriteFile(filepath.Join(zone0, "type"), []byte("x86_pkg_temp\n"), 0644)
	os.WriteFile(filepath.Join(zone0, "temp"), []byte("48000\n"), 0644)

	// Zone 1: Battery zone at 60°C — must be ignored even though it's hotter.
	zone1 := filepath.Join(dir, "thermal_zone1")
	os.MkdirAll(zone1, 0755)
	os.WriteFile(filepath.Join(zone1, "type"), []byte("battery\n"), 0644)
	os.WriteFile(filepath.Join(zone1, "temp"), []byte("60000\n"), 0644)

	got := readMaxThermalTemp(dir)
	if got != 48.0 {
		t.Errorf("readMaxThermalTemp() = %.1f°C, want 48.0°C (battery zone must be ignored)", got)
	}
}

func TestReadMaxThermalTemp_NoZones(t *testing.T) {
	dir := t.TempDir() // empty — no thermal_zone* subdirectories
	// Reset the sync.Once so the "no zones" warning can fire in this test
	// without being suppressed by a prior call in the same process.
	thermalWarnOnce = sync.Once{}

	got := readMaxThermalTemp(dir)
	if got != 0 {
		t.Errorf("readMaxThermalTemp() on empty dir = %.1f, want 0.0", got)
	}
}

func TestReadMaxThermalTemp_MultipleCPUZones(t *testing.T) {
	dir := t.TempDir()

	// Two CPU zones — function should return the max.
	for i, temp := range []string{"35000", "72000"} {
		zoneDir := filepath.Join(dir, fmt.Sprintf("thermal_zone%d", i))
		os.MkdirAll(zoneDir, 0755)
		os.WriteFile(filepath.Join(zoneDir, "type"), []byte("x86_pkg_temp\n"), 0644)
		os.WriteFile(filepath.Join(zoneDir, "temp"), []byte(temp+"\n"), 0644)
	}

	got := readMaxThermalTemp(dir)
	if got != 72.0 {
		t.Errorf("readMaxThermalTemp() = %.1f°C, want 72.0°C", got)
	}
}

// ── alertTriggers ─────────────────────────────────────────────────────────────

func TestAlertTriggers(t *testing.T) {
	orig := cfg
	defer func() { cfg = orig }()
	cfg.CPUUsageThreshold = 90
	cfg.CPUTempThresholdC = 85

	tests := []struct {
		cpu, temp float64
		want      string
	}{
		{95, 90, "cpu+temp"}, // both thresholds exceeded
		{95, 80, "cpu"},      // only CPU usage high
		{80, 90, "temp"},     // only temperature high
		{80, 80, "unknown"},  // neither — shouldn't happen in practice
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("cpu%.0f_temp%.0f", tt.cpu, tt.temp), func(t *testing.T) {
			if got := alertTriggers(tt.cpu, tt.temp); got != tt.want {
				t.Errorf("alertTriggers(%.0f, %.0f) = %q, want %q", tt.cpu, tt.temp, got, tt.want)
			}
		})
	}
}

// ── writeAlert ────────────────────────────────────────────────────────────────

func TestWriteAlert_CreatesFileWithCorrectName(t *testing.T) {
	orig := cfg
	origLast := lastAlert
	defer func() { cfg = orig; lastAlert = origLast }()

	alertDir := t.TempDir()
	cfg = Config{
		CPUUsageThreshold: 90,
		CPUTempThresholdC: 85,
		SustainMinutes:    5,
		AlertDir:          alertDir,
		LogFile:           "/tmp/test-monitor.log",
	}
	lastAlert = time.Time{}

	if err := writeAlert(95.0, 90.0); err != nil {
		t.Fatalf("writeAlert() unexpected error: %v", err)
	}

	entries, err := os.ReadDir(alertDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 alert file, got %d (err: %v)", len(entries), err)
	}
	name := entries[0].Name()

	// Filename must end with _cpu+temp.txt when both thresholds are exceeded.
	if !strings.HasSuffix(name, "_cpu+temp.txt") {
		t.Errorf("alert filename %q should end with _cpu+temp.txt", name)
	}
}

func TestWriteAlert_BodyMentionsTriggers(t *testing.T) {
	orig := cfg
	origLast := lastAlert
	defer func() { cfg = orig; lastAlert = origLast }()

	alertDir := t.TempDir()
	cfg = Config{
		CPUUsageThreshold: 90,
		CPUTempThresholdC: 85,
		SustainMinutes:    5,
		AlertDir:          alertDir,
		LogFile:           "/tmp/test-monitor.log",
	}

	if err := writeAlert(95.0, 90.0); err != nil {
		t.Fatalf("writeAlert() error: %v", err)
	}

	entries, _ := os.ReadDir(alertDir)
	body, _ := os.ReadFile(filepath.Join(alertDir, entries[0].Name()))
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "CPU usage") {
		t.Error("alert body should mention CPU usage")
	}
	if !strings.Contains(bodyStr, "CPU temp") {
		t.Error("alert body should mention CPU temp")
	}
}

func TestWriteAlert_SetsLastAlert(t *testing.T) {
	orig := cfg
	origLast := lastAlert
	defer func() { cfg = orig; lastAlert = origLast }()

	alertDir := t.TempDir()
	cfg = Config{
		CPUUsageThreshold: 90,
		CPUTempThresholdC: 85,
		AlertDir:          alertDir,
		LogFile:           "/tmp/test-monitor.log",
	}
	lastAlert = time.Time{}

	if err := writeAlert(95.0, 80.0); err != nil {
		t.Fatalf("writeAlert() error: %v", err)
	}
	if lastAlert.IsZero() {
		t.Error("lastAlert should be set after successful writeAlert()")
	}
}

func TestWriteAlert_BadDir(t *testing.T) {
	orig := cfg
	origLast := lastAlert
	defer func() { cfg = orig; lastAlert = origLast }()

	cfg = Config{
		CPUUsageThreshold: 90,
		CPUTempThresholdC: 85,
		AlertDir:          "/nonexistent/path/that/does/not/exist",
		LogFile:           "/tmp/test-monitor.log",
	}

	if err := writeAlert(95.0, 90.0); err == nil {
		t.Error("writeAlert() to non-existent dir should return an error")
	}
}
```

- [ ] **Step 3: Verify tests compile and fail correctly**

```bash
cd ~/projects/linux-system-monitor
go test ./... 2>&1 | head -30
```

Expected: All tests compile. Most FAIL because the stub functions return zero values. The test for `TestIsCPUThermalZone_MissingTypeFile` may pass (stub returns `false`, want `true` → fails). `TestConfigPath_Fallback` may pass. Everything else should fail.

- [ ] **Step 4: Commit scaffold**

```bash
git add main.go main_test.go
git commit -m "scaffold main.go stubs and write all unit tests"
```

---

## Task 3: Implement configPath(), loadConfig(), validateConfig()

**Files:**
- Modify: `main.go` (replace stubs for these three functions)

- [ ] **Step 1: Replace configPath() stub**

```go
// configPath resolves where to read the config file, checking in priority order:
//   1. $MONITOR_CONFIG environment variable — explicit override
//   2. /config/config.yaml — standard container mount point
//   3. ./config.yaml — fallback for local development
func configPath() string {
	if p := os.Getenv("MONITOR_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("/config/config.yaml"); err == nil {
		return "/config/config.yaml"
	}
	return "./config.yaml"
}
```

- [ ] **Step 2: Replace loadConfig() stub**

```go
// loadConfig reads the YAML config file and applies defaults for zero-valued fields.
// Uses log.Fatalf (stdlib, not file logger) because logger isn't open yet.
func loadConfig() {
	path := configPath()
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Could not read config from %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Invalid YAML in %s: %v", path, err)
	}

	// Apply safe defaults for fields left at Go zero-values (0 / "").
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
```

- [ ] **Step 3: Replace validateConfig() stub**

```go
// validateConfig checks all threshold values are in sensible ranges.
// Collects every error before calling log.Fatal so the user sees them all at once.
func validateConfig() {
	var errs []string

	if cfg.CPUUsageThreshold <= 0 || cfg.CPUUsageThreshold > 100 {
		errs = append(errs, "cpu_usage_threshold_percent must be between 1 and 100")
	}
	if cfg.CPUTempThresholdC <= 0 || cfg.CPUTempThresholdC > 150 {
		errs = append(errs, "cpu_temp_threshold_c must be between 1 and 150")
	}
	if cfg.SustainMinutes <= 0 {
		errs = append(errs, "sustain_minutes must be > 0")
	}
	if cfg.CheckIntervalSeconds <= 0 {
		errs = append(errs, "check_interval_seconds must be > 0")
	}
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
		log.Fatal("Fix config errors above and restart")
	}
}
```

- [ ] **Step 4: Run config-related tests**

```bash
cd ~/projects/linux-system-monitor
go test ./... -run "TestConfigPath" -v
```

Expected output:
```
--- PASS: TestConfigPath_EnvVar (0.00s)
--- PASS: TestConfigPath_Fallback (0.00s)
PASS
```

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "implement configPath, loadConfig, validateConfig"
```

---

## Task 4: Implement isCPUThermalZone() and readMaxThermalTemp()

**Files:**
- Modify: `main.go` (replace stubs for these two functions)

- [ ] **Step 1: Replace isCPUThermalZone() stub**

```go
// isCPUThermalZone returns true if the zone at tempPath is a CPU sensor.
// It reads the "type" file that is a sibling of "temp" in the zone directory.
// If the type file is missing or unreadable, returns true conservatively
// (include the zone rather than silently drop a potentially valid sensor).
func isCPUThermalZone(tempPath string) bool {
	// The zone directory contains both "temp" and "type" files.
	// e.g. /sys/class/thermal/thermal_zone0/temp → /sys/class/thermal/thermal_zone0/type
	typeFile := filepath.Join(filepath.Dir(tempPath), "type")
	data, err := os.ReadFile(typeFile)
	if err != nil {
		// Can't determine the zone type — include it conservatively.
		return true
	}

	zoneType := strings.TrimSpace(string(data))

	// Known CPU thermal zone type identifiers across architectures:
	//   x86_pkg_temp — Intel CPU package (most common on x86)
	//   cpu-thermal   — ARM/Raspberry Pi CPU
	//   cpu0-thermal  — Some ARM SoCs
	//   soc-thermal   — Generic SoC thermal zone (usually CPU)
	//   acpitz        — ACPI thermal zone (typically CPU on desktop/laptop)
	cpuTypes := []string{
		"x86_pkg_temp",
		"cpu-thermal",
		"cpu0-thermal",
		"soc-thermal",
		"acpitz",
	}
	for _, t := range cpuTypes {
		if zoneType == t {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Replace readMaxThermalTemp() stub**

```go
// readMaxThermalTemp scans thermal zones under baseDir and returns the highest
// CPU zone temperature in degrees Celsius.
//
// baseDir in production: "/sys/class/thermal"
// baseDir in tests:      a temp directory mirroring the /sys structure
//
// The kernel reports temperature in millidegrees Celsius (48000 = 48°C).
func readMaxThermalTemp(baseDir string) float64 {
	// Glob expands to every thermal_zone*/temp path under baseDir.
	zones, err := filepath.Glob(filepath.Join(baseDir, "thermal_zone*/temp"))
	if err != nil || len(zones) == 0 {
		// sync.Once ensures this warning appears exactly once per process run,
		// not on every tick — avoids log spam on systems with no sensors (VMs).
		thermalWarnOnce.Do(func() {
			logger.Println("No thermal zones found — temperature monitoring disabled (normal inside VMs)")
		})
		return 0
	}

	maxTemp := 0.0
	for _, path := range zones {
		// Skip zones that are not CPU sensors (battery, NVMe, PCH, etc.).
		if !isCPUThermalZone(path) {
			continue
		}

		data, err := os.ReadFile(path)
		if err != nil {
			continue // zone may be offline or permission-denied; skip silently
		}

		// TrimSpace removes the trailing newline before parsing.
		milliC, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}

		// Convert millidegrees → degrees Celsius.
		if tempC := milliC / 1000.0; tempC > maxTemp {
			maxTemp = tempC
		}
	}
	return maxTemp
}
```

- [ ] **Step 3: Run thermal tests**

```bash
cd ~/projects/linux-system-monitor
go test ./... -run "TestIsCPU|TestReadMax" -v
```

Expected output:
```
--- PASS: TestIsCPUThermalZone/x86_pkg_temp (0.00s)
--- PASS: TestIsCPUThermalZone/cpu-thermal (0.00s)
--- PASS: TestIsCPUThermalZone/cpu0-thermal (0.00s)
--- PASS: TestIsCPUThermalZone/soc-thermal (0.00s)
--- PASS: TestIsCPUThermalZone/acpitz (0.00s)
--- PASS: TestIsCPUThermalZone/battery (0.00s)
--- PASS: TestIsCPUThermalZone/pch_skylake (0.00s)
--- PASS: TestIsCPUThermalZone/INT3400_Thermal (0.00s)
--- PASS: TestIsCPUThermalZone_MissingTypeFile (0.00s)
--- PASS: TestReadMaxThermalTemp_FiltersNonCPU (0.00s)
--- PASS: TestReadMaxThermalTemp_NoZones (0.00s)
--- PASS: TestReadMaxThermalTemp_MultipleCPUZones (0.00s)
PASS
```

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "implement isCPUThermalZone and readMaxThermalTemp with CPU-type filtering"
```

---

## Task 5: Implement alertTriggers() and writeAlert()

**Files:**
- Modify: `main.go` (replace stubs for these two functions)

- [ ] **Step 1: Replace alertTriggers() stub**

```go
// alertTriggers returns a slug encoding which threshold(s) triggered the alert.
// Used in both the alert filename and the email subject.
//
// Possible return values: "cpu", "temp", "cpu+temp", "unknown"
// "unknown" should never occur in practice (isHigh is true when called).
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
```

- [ ] **Step 2: Replace writeAlert() stub**

```go
// writeAlert writes an alert .txt file to cfg.AlertDir.
// The filename encodes the timestamp and which threshold(s) triggered:
//   2026-04-16_14-32-00_cpu+temp.txt
//
// entrypoint.sh watches cfg.AlertDir with inotifywait. When a new file
// appears, the watcher pipes it to send-report for email delivery.
//
// lastAlert is set on success so the cooldown logic in the main loop works.
// Returns an error if the file cannot be written (e.g. directory not mounted).
func writeAlert(cpuUsage, cpuTempC float64) error {
	now := time.Now()
	trigger := alertTriggers(cpuUsage, cpuTempC)

	// Filename: YYYY-MM-DD_HH-MM-SS_<trigger>.txt
	// Colons are not used in the time portion to avoid issues on some filesystems.
	filename := fmt.Sprintf("%s_%s.txt", now.Format("2006-01-02_15-04-05"), trigger)
	path := filepath.Join(cfg.AlertDir, filename)

	// Build a list of human-readable reasons for the alert body.
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

Condition was sustained for over %d minute(s).
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

	// WriteFile is atomic enough for our purposes: the file is written and
	// closed before inotifywait fires the close_write event.
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		return fmt.Errorf("write alert file %s: %w", path, err)
	}

	// Record when we last alerted so the cooldown check in the main loop works.
	lastAlert = now
	logger.Printf("Alert written: %s", filename)
	return nil
}
```

- [ ] **Step 3: Run alert tests**

```bash
cd ~/projects/linux-system-monitor
go test ./... -run "TestAlertTriggers|TestWriteAlert" -v
```

Expected output:
```
--- PASS: TestAlertTriggers/cpu95_temp90 (0.00s)
--- PASS: TestAlertTriggers/cpu95_temp80 (0.00s)
--- PASS: TestAlertTriggers/cpu80_temp90 (0.00s)
--- PASS: TestAlertTriggers/cpu80_temp80 (0.00s)
--- PASS: TestWriteAlert_CreatesFileWithCorrectName (0.00s)
--- PASS: TestWriteAlert_BodyMentionsTriggers (0.00s)
--- PASS: TestWriteAlert_SetsLastAlert (0.00s)
--- PASS: TestWriteAlert_BadDir (0.00s)
PASS
```

- [ ] **Step 4: Run full test suite**

```bash
go test ./... -v 2>&1 | tail -5
```

Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "implement alertTriggers and writeAlert (drop-directory model)"
```

---

## Task 6: Complete main.go — remaining functions and main()

**Files:**
- Modify: `main.go` (replace remaining stubs; add full main())

- [ ] **Step 1: Replace checkLogDir() stub**

```go
// checkLogDir ensures the log file's parent directory exists and is writable.
// Called before opening the log file so failures produce a clear message.
func checkLogDir() {
	dir := filepath.Dir(cfg.LogFile)

	// MkdirAll creates the directory and any missing parents.
	// 0755 = owner rwx, group+other rx — standard for system directories.
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatalf("Cannot create log directory %s: %v", dir, err)
	}

	// Probe writability by creating and immediately removing a temp file.
	probe := filepath.Join(dir, ".write_probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Log directory %s is not writable: %v", dir, err)
	}
	f.Close()
	os.Remove(probe)
}
```

- [ ] **Step 2: Replace getMetrics() stub**

```go
// getMetrics samples CPU usage (averaged over 500ms) and max CPU temperature.
// Returns an error only if cpu.Percent fails; temperature errors are non-fatal
// (temp returns 0, which is below any valid threshold, so no false alerts).
func getMetrics() (cpuUsage, cpuTempC float64, err error) {
	// cpu.Percent blocks for 500ms then returns aggregate usage across all cores.
	// false = don't split per-core; we want a single system-wide percentage.
	usage, err := cpu.Percent(500*time.Millisecond, false)
	if err != nil {
		return 0, 0, err
	}
	if len(usage) > 0 {
		cpuUsage = usage[0]
	}

	// readMaxThermalTemp uses the real kernel path in production.
	cpuTempC = readMaxThermalTemp("/sys/class/thermal")
	return cpuUsage, cpuTempC, nil
}
```

- [ ] **Step 3: Replace checkSelfResources() stub**

```go
// checkSelfResources returns this daemon's CPU% and RSS memory in MB.
// Uses the persistent selfProc handle so CPUPercent() measures the delta
// since the previous call — accurate without requiring a sleep.
func checkSelfResources() (cpuPct, memMB float64, err error) {
	cpuPct, err = selfProc.CPUPercent()
	if err != nil {
		return 0, 0, err
	}

	// MemoryInfo returns RSS (resident set size) — actual RAM pages in use.
	mem, err := selfProc.MemoryInfo()
	if err != nil {
		return 0, 0, err
	}

	// Convert bytes → megabytes for comparison with cfg.SelfMaxMemMB.
	memMB = float64(mem.RSS) / 1024 / 1024
	return cpuPct, memMB, nil
}
```

- [ ] **Step 4: Replace main() stub**

```go
func main() {
	loadConfig()
	validateConfig()

	ownPID = os.Getpid()
	checkLogDir()

	// Open (or create) the log file in append mode so history survives restarts.
	logFile, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Cannot open log file: %v", err)
	}
	defer logFile.Close()

	logger = log.New(logFile, "MONITOR: ", log.LstdFlags|log.Lshortfile)
	logger.Printf("Linux System Monitor started — PID %d", ownPID)

	// Build a persistent process handle for self-monitoring.
	// Keeping one handle lets CPUPercent() measure CPU ticks elapsed since
	// the previous call, giving accurate readings without a blocking sleep.
	selfProc, err = process.NewProcess(int32(ownPID))
	if err != nil {
		log.Fatalf("Cannot get self process handle: %v", err)
	}
	// Prime the CPU baseline — the first call always returns 0; every subsequent
	// call returns real CPU% since this call. Discard the priming result.
	_, _ = selfProc.CPUPercent()

	// Deferred panic recovery: if anything unexpected panics, log it with a
	// full stack trace before exiting. Note: log.Fatal/Fatalf calls os.Exit(1)
	// directly and does NOT trigger deferred functions — only real panics reach here.
	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC recovered: %v\nStack:\n%s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	// context.WithCancel gives a clean way to stop the ticker loop on signal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Listen for SIGINT (Ctrl+C) and SIGTERM (podman stop / systemd).
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
			// alive even when nothing is wrong.
			if heartbeat%10 == 0 {
				logger.Println("Heartbeat — monitor still alive")
			}

			// ── 1. SYSTEM METRICS ─────────────────────────────────────────
			cpuUsage, cpuTempC, metricErr := getMetrics()
			if metricErr != nil {
				consecutiveErrors++
				logger.Printf("Metric error (%d/%d): %v",
					consecutiveErrors, cfg.MaxConsecutiveErrors, metricErr)
				if consecutiveErrors >= cfg.MaxConsecutiveErrors {
					// Too many failures likely means a driver or kernel issue.
					// Exit so the supervisor can restart us with a clean state.
					logger.Fatal("Too many consecutive errors — exiting for restart")
				}
				continue
			}
			consecutiveErrors = 0

			logger.Printf("Check → CPU %.1f%% | Temp %.1f°C", cpuUsage, cpuTempC)

			// isHigh fires when EITHER threshold is exceeded (OR logic).
			isHigh := (cpuTempC >= cfg.CPUTempThresholdC) || (cpuUsage >= cfg.CPUUsageThreshold)
			now := time.Now()

			if isHigh {
				if highStart.IsZero() {
					highStart = now
				}
				sustained := time.Since(highStart) >= time.Duration(cfg.SustainMinutes)*time.Minute
				cooledDown := time.Since(lastAlert) >= time.Duration(cfg.AlertCooldownMinutes)*time.Minute
				if sustained && cooledDown {
					if err := writeAlert(cpuUsage, cpuTempC); err != nil {
						logger.Printf("Failed to write alert file: %v", err)
					}
					// Reset so the next sustained period can trigger a new alert.
					highStart = time.Time{}
				}
			} else {
				// Condition cleared — reset the sustain timer.
				highStart = time.Time{}
			}

			// ── 2. SELF-RESOURCE SAFEGUARD ────────────────────────────────
			selfCPU, selfMem, selfErr := checkSelfResources()
			if selfErr != nil {
				// Bug fix: reset selfHighStart so stale timers don't accumulate
				// if self-resource checks start failing after a high period.
				selfHighStart = time.Time{}
				logger.Printf("Self-resource check failed (non-fatal): %v", selfErr)
				continue
			}

			selfOver := (selfCPU >= cfg.SelfMaxCPUPercent) || (selfMem >= cfg.SelfMaxMemMB)
			if selfOver {
				if selfHighStart.IsZero() {
					selfHighStart = now
				}
				// Exit cleanly if over our own limits for too long.
				// Podman's restart policy brings us back with a fresh process.
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
// • Drop-directory alerting: the daemon writes files; a separate watcher
//   delivers them. This fully decouples monitoring from delivery — a hung
//   send-report cannot block or crash the monitor.
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
// • selfHighStart reset on error: if the self-resource check fails repeatedly,
//   the stale timer is cleared so it cannot trigger a false overload exit.
```

- [ ] **Step 5: Verify everything builds and all tests still pass**

```bash
cd ~/projects/linux-system-monitor
go build ./... && go test ./... -v 2>&1 | tail -10
```

Expected:
```
--- PASS: TestWriteAlert_BadDir (0.00s)
--- PASS: TestWriteAlert_SetsLastAlert (0.00s)
--- PASS: TestWriteAlert_BodyMentionsTriggers (0.00s)
--- PASS: TestWriteAlert_CreatesFileWithCorrectName (0.00s)
PASS
ok  	linux-system-monitor	0.XXXs
```

- [ ] **Step 6: Commit**

```bash
git add main.go
git commit -m "complete main.go: remaining functions and main loop with all bug fixes"
```

---

## Task 7: Create entrypoint.sh

**Files:**
- Create: `entrypoint.sh`

- [ ] **Step 1: Write entrypoint.sh**

```sh
#!/bin/sh
# entrypoint.sh — PID 1 for the linux-system-monitor container.
#
# Responsibilities:
#   1. Ensure /alerts and /logs directories exist inside the container.
#   2. Start an inotifywait watcher in the background. When the Go monitor
#      writes an alert file to /alerts/, the watcher pipes it to send-report.
#   3. Replace this shell with the Go binary via exec, so the monitor
#      becomes PID 1 and receives SIGTERM directly from 'podman stop'.
#
# Alert filenames follow the pattern: YYYY-MM-DD_HH-MM-SS_<trigger>.txt
# where <trigger> is one of: cpu, temp, cpu+temp

# set -e exits immediately if any setup command fails.
# We use it for the mkdir section; the watcher loop runs independently.
set -e

# Ensure runtime directories exist. These may already exist from the image
# layer or the bind mount, but mkdir -p is idempotent.
mkdir -p /alerts /logs

# ── Start the inotify watcher (background) ───────────────────────────────────
# inotifywait -m: monitor continuously (don't exit after first event).
# -e close_write: fire only when a file is fully written and closed — not
#   on the open or partial-write events, which would give us incomplete files.
# --format '%f': print only the bare filename (not the full path or event name).
# 2>/dev/null: suppress inotifywait's startup banner from container logs.
#
# The watcher loop is started with & (background) before we exec the binary.
inotifywait -m -e close_write --format '%f' /alerts/ 2>/dev/null |
while IFS= read -r filename; do
    # Derive the email subject from the trigger suffix in the filename.
    # e.g. "2026-04-16_14-32-00_cpu+temp.txt" → trigger="cpu+temp"
    # sed pattern: strip everything up to and including the last underscore,
    # then strip the .txt extension.
    trigger=$(echo "$filename" | sed 's/.*_\(.*\)\.txt$/\1/')
    subject="Linux Monitor Alert: $trigger threshold exceeded"

    # Pipe the alert file body to send-report with the subject as argument.
    # The & runs delivery in a subshell so a slow or hung send-report cannot
    # block the watcher loop from processing the next alert file.
    send-report "$subject" < "/alerts/$filename" &
done &
# The & above backgrounds the entire pipeline (inotifywait + while loop).

# ── Replace shell with Go binary (foreground) ────────────────────────────────
# exec replaces this shell process with the Go binary.
# Effects:
#   • The Go binary becomes PID 1 in the container.
#   • 'podman stop' sends SIGTERM directly to the binary (not to a shell wrapper).
#   • The existing signal handler in main.go shuts down cleanly.
exec /usr/local/bin/linux-system-monitor
```

- [ ] **Step 2: Make it executable**

```bash
chmod +x ~/projects/linux-system-monitor/entrypoint.sh
```

- [ ] **Step 3: Verify the shebang and line endings are correct**

```bash
head -1 ~/projects/linux-system-monitor/entrypoint.sh
file ~/projects/linux-system-monitor/entrypoint.sh
```

Expected:
```
#!/bin/sh
entrypoint.sh: POSIX shell script, ASCII text executable
```

If the file shows "CRLF line terminators", run: `sed -i 's/\r//' entrypoint.sh`

- [ ] **Step 4: Commit**

```bash
cd ~/projects/linux-system-monitor
git add entrypoint.sh
git commit -m "add entrypoint.sh: inotify watcher + exec Go binary as PID 1"
```

---

## Task 8: Create Containerfile

**Files:**
- Create: `Containerfile`

- [ ] **Step 1: Write Containerfile**

```dockerfile
# Containerfile — multi-stage build for linux-system-monitor
#
# Build targets:
#   podman build -t linux-system-monitor .
#     → builds the 'final' stage (default): minimal Alpine + msmtp + inotify-tools
#
#   podman build --target=debug -t linux-system-monitor-debug .
#     → builds the 'debug' stage: same as final + bash, curl, strace for inspection

# ── Stage 1: Builder ─────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

WORKDIR /build

# Copy module manifests before source code so Podman can cache the
# go mod download layer independently. If only main.go changes,
# dependencies are not re-downloaded on rebuild.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a fully static binary.
# CGO_ENABLED=0: pure Go build — no glibc or C runtime dependency.
# -ldflags="-w -s": strip debug info and symbol table, reduces binary size.
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-w -s" -o linux-system-monitor .

# ── Stage 2: Debug (optional) ─────────────────────────────────────────────
# Build with: podman build --target=debug -t linux-system-monitor-debug .
# Use with:   podman run -it --rm linux-system-monitor-debug sh
FROM alpine:3.19 AS debug

# All runtime packages plus shell tools for interactive troubleshooting.
RUN apk add --no-cache \
    msmtp \
    inotify-tools \
    bash \
    curl \
    strace

COPY --from=builder /build/linux-system-monitor /usr/local/bin/linux-system-monitor
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/linux-system-monitor

# Pre-create runtime directories. Bind mounts will shadow them at run time.
RUN mkdir -p /alerts /logs /config

ENTRYPOINT ["/entrypoint.sh"]

# ── Stage 3: Final (default) ──────────────────────────────────────────────
# This is the default build target (last FROM in the file).
FROM alpine:3.19 AS final

# Only the two packages needed at runtime.
# msmtp: SMTP client used by the bind-mounted send-report script.
# inotify-tools: provides inotifywait for the alert watcher in entrypoint.sh.
RUN apk add --no-cache \
    msmtp \
    inotify-tools

COPY --from=builder /build/linux-system-monitor /usr/local/bin/linux-system-monitor
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/linux-system-monitor

# Pre-create runtime directories. Bind mounts will shadow them at run time.
RUN mkdir -p /alerts /logs /config

ENTRYPOINT ["/entrypoint.sh"]
```

- [ ] **Step 2: Build the image and check for errors**

```bash
cd ~/projects/linux-system-monitor
podman build -t linux-system-monitor . 2>&1
```

Expected: build completes with no errors. Final line similar to:
```
COMMIT linux-system-monitor
--> <image-id>
Successfully tagged localhost/linux-system-monitor:latest
```

- [ ] **Step 3: Check final image size**

```bash
podman images linux-system-monitor
```

Expected: image size roughly 30–50 MB (Alpine base + msmtp + inotify-tools + static Go binary).

- [ ] **Step 4: Verify the binary runs inside the image**

```bash
podman run --rm linux-system-monitor /usr/local/bin/linux-system-monitor --help 2>&1 || true
```

Expected: either a "flag provided but not defined" error (the binary has no --help flag) or a "Could not read config" message — both confirm the binary executes successfully inside the container.

- [ ] **Step 5: Commit**

```bash
git add Containerfile
git commit -m "add multi-stage Containerfile: Go builder → Alpine final + debug target"
```

---

## Task 9: Host setup and first run

**Files:**
- No repo files changed in this task — host filesystem setup only.

- [ ] **Step 1: Create host directories**

```bash
mkdir -p ~/.config/linux-system-monitor
mkdir -p ~/.local/share/linux-system-monitor/alerts
mkdir -p ~/.local/share/linux-system-monitor/logs
```

- [ ] **Step 2: Copy config template to host config location**

```bash
cp ~/projects/linux-system-monitor/config.yaml \
   ~/.config/linux-system-monitor/config.yaml
```

- [ ] **Step 3: Verify all bind-mount source paths exist**

```bash
ls -la ~/.config/linux-system-monitor/config.yaml
ls -la ~/.local/share/linux-system-monitor/alerts/
ls -la ~/.local/share/linux-system-monitor/logs/
ls -la ~/bin/send-report
ls -la ~/.msmtprc
ls -la /sys/class/thermal/
```

All six must exist before running the container. Fix any missing paths before continuing.

- [ ] **Step 4: Run the container**

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

- [ ] **Step 5: Verify the container started and is logging**

```bash
podman ps | grep linux-monitor
```

Expected:
```
<id>  localhost/linux-system-monitor:latest  /entrypoint.sh  X seconds ago  Up X seconds  linux-monitor
```

```bash
sleep 3 && podman logs linux-monitor
```

Expected (log entries from the file logger):
```
MONITOR: <timestamp> main.go:XX: Linux System Monitor started — PID <pid>
```

- [ ] **Step 6: Verify the log file is appearing on the host**

```bash
cat ~/.local/share/linux-system-monitor/logs/monitor.log
```

Expected: same content as `podman logs linux-monitor` (both read the same file).

- [ ] **Step 7: Smoke-test alert delivery manually**

Drop a fake alert file into the alerts directory and confirm the watcher delivers it:

```bash
echo "Test alert body from manual drop" > \
  ~/.local/share/linux-system-monitor/alerts/2026-04-16_00-00-00_cpu.txt
```

Wait ~5 seconds, then check your email for subject "Linux Monitor Alert: cpu threshold exceeded". If the email arrives, the inotify watcher → send-report → msmtp chain is working end-to-end.

- [ ] **Step 8: Stop the container cleanly**

```bash
podman stop linux-monitor
podman logs linux-monitor 2>&1 | tail -3
```

Expected final log line:
```
MONITOR: <timestamp> main.go:XX: Shutting down gracefully
```

---

## Task 10: Update README.md

**Files:**
- Rewrite: `README.md`

- [ ] **Step 1: Write README.md**

```markdown
# linux-system-monitor

A Go daemon that monitors CPU usage and temperature, sends email alerts when
thresholds are sustained, and watches its own resource usage so it can't become
the problem it's watching for. Runs in a Podman container.

## What it does

- Polls CPU usage and temperature every configurable interval
- Alerts only when a threshold is **sustained** for N minutes (no false spikes)
- Cooldown between alerts to prevent spam
- Only reads **CPU thermal zones** — battery, NVMe, and PCH sensors ignored
- Writes alert files to a drop directory; `entrypoint.sh` delivers them via `send-report`
- Self-safeguard: exits cleanly if the daemon itself uses too much CPU or RAM

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
cat ~/.local/share/linux-system-monitor/logs/monitor.log       # log file

# Debug build (has bash, curl, strace)
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
| `alert_cooldown_minutes` | 30 | Min time between alerts |
| `alert_dir` | `/alerts` | Drop directory (match your --volume) |
| `log_file` | `/logs/monitor.log` | Log path (match your --volume) |

Config path resolution: `$MONITOR_CONFIG` → `/config/config.yaml` → `./config.yaml`

## Alert format

Alert files are dropped to `~/.local/share/linux-system-monitor/alerts/` with names like:

```
2026-04-16_14-32-00_cpu+temp.txt   # both thresholds exceeded
2026-04-16_14-32-00_cpu.txt        # only CPU usage exceeded
2026-04-16_14-32-00_temp.txt       # only temperature exceeded
```

`entrypoint.sh` watches this directory with `inotifywait` and pipes each file to
`send-report` for email delivery. The Go binary never calls `send-report` directly.

## Requirements

- `~/bin/send-report` — executable script (piped stdin → email via msmtp)
- `~/.msmtprc` — msmtp config with a `gmail` account
- `/sys/class/thermal` — present on physical hardware; absent in VMs (temperature disabled automatically)
```

- [ ] **Step 2: Commit and push**

```bash
cd ~/projects/linux-system-monitor
git add README.md
git commit -m "rewrite README for container-first workflow"
git push
```

---

## Self-Review

**Spec coverage check:**

| Spec requirement | Covered by |
|---|---|
| Containerfile multi-stage (builder + debug + final) | Task 8 |
| Alpine final image with msmtp + inotify-tools | Task 8 |
| entrypoint.sh as PID 1 | Task 7 |
| Drop-directory alert model | Tasks 5, 7 |
| config.yaml with alert_dir, log_file | Task 1 |
| Config path: $MONITOR_CONFIG → /config → ./ | Task 3 |
| Thermal zone CPU filtering | Task 4 |
| alertTriggers() slug in filename and body | Task 5 |
| selfHighStart reset on selfErr | Task 6 |
| All tests pass | Tasks 3–6 |
| Host directory setup | Task 9 |
| podman run one-liner | Tasks 9, 10 |
| README updated | Task 10 |

**No placeholders found.** All steps have concrete code or commands.

**Type consistency:** `alertTriggers()` returns `string` — used in `writeAlert()` as `trigger` variable in `fmt.Sprintf`. `readMaxThermalTemp(baseDir string)` called as `readMaxThermalTemp("/sys/class/thermal")` in `getMetrics()` and `readMaxThermalTemp(dir)` in tests. All consistent.
