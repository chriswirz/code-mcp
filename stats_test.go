package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFormatBytes(t *testing.T) {
	cases := map[uint64]string{
		0:                 "0 B",
		1023:              "1023 B",
		1024:              "1.0 KiB",
		1536:              "1.5 KiB",
		1 << 20:           "1.0 MiB",
		1 << 30:           "1.0 GiB",
		1<<40 + 1<<39:     "1.5 TiB",
		5 * (1 << 30) / 2: "2.5 GiB",
	}
	for n, want := range cases {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// diskUsage should work on any real, existing directory on the host running
// the test - the platform-specific syscall is the point of the test.
func TestDiskUsageOnWorkingDirectory(t *testing.T) {
	total, free, err := diskUsage(t.TempDir())
	if err != nil {
		t.Fatalf("diskUsage: %v", err)
	}
	if total == 0 {
		t.Error("total should be nonzero for a real directory")
	}
	if free > total {
		t.Errorf("free (%d) > total (%d)", free, total)
	}
}

func TestDiskStatsSnapshot(t *testing.T) {
	stats := diskStatsSnapshot(t.TempDir())
	if stats.TotalBytes == 0 {
		t.Fatal("expected a nonzero total for a real directory")
	}
	if stats.UsedBytes+stats.FreeBytes != stats.TotalBytes {
		t.Errorf("used (%d) + free (%d) != total (%d)", stats.UsedBytes, stats.FreeBytes, stats.TotalBytes)
	}
	if stats.UsedPercent < 0 || stats.UsedPercent > 100 {
		t.Errorf("used percent out of range: %v", stats.UsedPercent)
	}
}

func TestDiskStatsSnapshotOnMissingPath(t *testing.T) {
	stats := diskStatsSnapshot("Z:\\this\\path\\does\\not\\exist\\anywhere")
	if stats.TotalBytes != 0 {
		t.Errorf("expected a zero-value snapshot for a bad path, got %+v", stats)
	}
}

// cpuStatsSnapshot is host-dependent, so this only checks the invariants that
// must hold on every supported platform, not particular values.
func TestCPUStatsSnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stats := cpuStatsSnapshot(ctx)
	if stats.LogicalCores <= 0 {
		t.Errorf("LogicalCores = %d, want > 0", stats.LogicalCores)
	}
	if stats.UsagePercent != nil && (*stats.UsagePercent < 0 || *stats.UsagePercent > 100) {
		t.Errorf("UsagePercent out of range: %v", *stats.UsagePercent)
	}
}

// gpuStatsSnapshot and gatherSystemStats must not hang or panic even where
// there is no GPU and none of the fallback tools exist; every real
// assertion about specific values is left to the platform-specific probes.
func TestGatherSystemStatsDoesNotPanicOrHang(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stats := gatherSystemStats(ctx, t.TempDir())
	if stats.CPU.LogicalCores <= 0 {
		t.Errorf("LogicalCores = %d, want > 0", stats.CPU.LogicalCores)
	}
	if summary := stats.summarize(); !strings.Contains(summary, "CPU") || !strings.Contains(summary, "Disk") {
		t.Errorf("summary is missing expected sections: %q", summary)
	}
}

func TestSystemStatsSummarizeOmitsMissingSections(t *testing.T) {
	stats := systemStats{CPU: cpuStats{LogicalCores: 4}}
	summary := stats.summarize()
	if strings.Contains(summary, "GPU") {
		t.Errorf("should not mention GPU when none were found: %q", summary)
	}
	if !strings.Contains(summary, "could not be determined") {
		t.Errorf("expected the disk section to say it could not be determined: %q", summary)
	}
}
