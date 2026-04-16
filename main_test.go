// main_test.go — unit tests for pure and file-based functions in main.go.
//
// Integration with real /proc and /sys is not tested here; verify those manually
// by running the container and watching podman logs.
//
// TestMain initialises a discard logger so any function that calls logger.Println
// doesn't panic during tests. Real logging goes to /logs/monitor.log at runtime.

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

// TestMain sets up shared test state. It runs before any test in this package.
func TestMain(m *testing.M) {
	// Initialise a logger that discards all output so test output stays clean.
	// Functions like readMaxThermalTemp call logger.Println and would panic
	// without this.
	logger = log.New(io.Discard, "", 0)
	os.Exit(m.Run())
}

// ── configPath ────────────────────────────────────────────────────────────────

func TestConfigPath_EnvVar(t *testing.T) {
	// t.Setenv automatically restores the original value when the test ends.
	t.Setenv("MONITOR_CONFIG", "/custom/path.yaml")
	if got := configPath(); got != "/custom/path.yaml" {
		t.Errorf("configPath() = %q, want /custom/path.yaml", got)
	}
}

func TestConfigPath_Fallback(t *testing.T) {
	// Ensure the env var is not set so we exercise the fallback logic.
	os.Unsetenv("MONITOR_CONFIG")
	got := configPath()
	// Depending on whether /config/config.yaml exists on this host, the result
	// is either that container path or the local development fallback.
	if got != "/config/config.yaml" && got != "./config.yaml" {
		t.Errorf("configPath() = %q, not a recognised fallback value", got)
	}
}

// ── isCPUThermalZone ─────────────────────────────────────────────────────────

func TestIsCPUThermalZone(t *testing.T) {
	// Build a fake /sys/class/thermal/thermal_zone0/ in a temp directory so
	// the test never touches real hardware paths.
	dir := t.TempDir()
	zoneDir := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zoneDir, 0755); err != nil {
		t.Fatal(err)
	}
	// isCPUThermalZone takes the path to the "temp" file and reads the
	// sibling "type" file from the same directory.
	tempPath := filepath.Join(zoneDir, "temp")
	typeFile := filepath.Join(zoneDir, "type")

	tests := []struct {
		zoneType string
		want     bool
	}{
		{"x86_pkg_temp", true},     // Intel CPU package sensor
		{"cpu-thermal", true},      // ARM / Raspberry Pi CPU
		{"cpu0-thermal", true},     // Some ARM SoCs
		{"soc-thermal", true},      // Generic SoC (usually CPU)
		{"acpitz", true},           // ACPI thermal — typically CPU
		{"battery", false},         // Battery sensor — must be ignored
		{"pch_skylake", false},     // Platform Controller Hub — must be ignored
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
	// include the zone rather than silently drop a potentially valid sensor.
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "temp") // no "type" file created
	if got := isCPUThermalZone(tempPath); !got {
		t.Error("isCPUThermalZone with missing type file should return true (conservative inclusion)")
	}
}

// ── readMaxThermalTemp ────────────────────────────────────────────────────────

func TestReadMaxThermalTemp_FiltersNonCPU(t *testing.T) {
	dir := t.TempDir()

	// Zone 0: CPU zone at 48°C — should be included in the max.
	zone0 := filepath.Join(dir, "thermal_zone0")
	os.MkdirAll(zone0, 0755)
	os.WriteFile(filepath.Join(zone0, "type"), []byte("x86_pkg_temp\n"), 0644)
	os.WriteFile(filepath.Join(zone0, "temp"), []byte("48000\n"), 0644)

	// Zone 1: Battery zone at 60°C — must be ignored even though it is hotter.
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
	dir := t.TempDir() // empty directory — no thermal_zone* subdirs
	// Reset sync.Once so the "no zones" warning can fire in this test without
	// being suppressed by an earlier call in the same process.
	thermalWarnOnce = sync.Once{}

	got := readMaxThermalTemp(dir)
	if got != 0 {
		t.Errorf("readMaxThermalTemp() on empty dir = %.1f, want 0.0", got)
	}
}

func TestReadMaxThermalTemp_MultipleCPUZones(t *testing.T) {
	dir := t.TempDir()

	// Two CPU zones — the function should return the highest temperature.
	for i, milliC := range []string{"35000", "72000"} {
		zoneDir := filepath.Join(dir, fmt.Sprintf("thermal_zone%d", i))
		os.MkdirAll(zoneDir, 0755)
		os.WriteFile(filepath.Join(zoneDir, "type"), []byte("x86_pkg_temp\n"), 0644)
		os.WriteFile(filepath.Join(zoneDir, "temp"), []byte(milliC+"\n"), 0644)
	}

	got := readMaxThermalTemp(dir)
	if got != 72.0 {
		t.Errorf("readMaxThermalTemp() = %.1f°C, want 72.0°C (should return max of all CPU zones)", got)
	}
}

// ── alertTriggers ─────────────────────────────────────────────────────────────

func TestAlertTriggers(t *testing.T) {
	// Save and restore global cfg so this test doesn't affect other tests.
	orig := cfg
	defer func() { cfg = orig }()
	cfg.CPUUsageThreshold = 90
	cfg.CPUTempThresholdC = 85

	tests := []struct {
		cpu, temp float64
		want      string
	}{
		{95, 90, "cpu+temp"}, // both thresholds exceeded simultaneously
		{95, 80, "cpu"},      // only CPU usage exceeded
		{80, 90, "temp"},     // only temperature exceeded
		{80, 80, "unknown"},  // neither — shouldn't occur in practice
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

	// Filename must end with _cpu+temp.txt when both thresholds are exceeded.
	if !strings.HasSuffix(entries[0].Name(), "_cpu+temp.txt") {
		t.Errorf("alert filename %q should end with _cpu+temp.txt", entries[0].Name())
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
	lastAlert = time.Time{} // ensure it starts zeroed

	if err := writeAlert(95.0, 80.0); err != nil {
		t.Fatalf("writeAlert() error: %v", err)
	}
	if lastAlert.IsZero() {
		t.Error("lastAlert should be set after a successful writeAlert() call")
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

	// Writing to a non-existent directory must return an error, not panic.
	if err := writeAlert(95.0, 90.0); err == nil {
		t.Error("writeAlert() to non-existent dir should return an error")
	}
}
