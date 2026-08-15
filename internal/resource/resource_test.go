package resource

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlanConstrainedHost(t *testing.T) {
	s := Snapshot{
		DiskFree:        6 * GiB,
		DiskTotal:       100 * GiB,
		MemoryLimit:     2 * GiB,
		MemoryCurrent:   512 * MiB,
		MemoryAvailable: 1536 * MiB,
		CPUQuota:        0.5,
	}
	l := DefaultLimits()
	l.MinFree = 1 * GiB
	p, err := Plan(s, l)
	if err != nil {
		t.Fatal(err)
	}
	if p.VolumeBytes != 512*MiB { // 5 GiB reserve (5%), leaving 1 GiB for two buffers
		t.Fatalf("volume=%d, want %d", p.VolumeBytes, 512*MiB)
	}
	if p.MaxConcurrency != 8 {
		t.Fatalf("concurrency=%d, want 8", p.MaxConcurrency)
	}
}

func TestPlanRejectsUnsafeDisk(t *testing.T) {
	s := Snapshot{DiskFree: 1100 * MiB, DiskTotal: 20 * GiB, MemoryAvailable: GiB, CPUQuota: 1}
	l := DefaultLimits()
	l.MinFree = GiB
	if _, err := Plan(s, l); err == nil {
		t.Fatal("expected low-disk error")
	}
}

func TestPlanRejectsFixedVolumeAboveCurrentSafeCap(t *testing.T) {
	s := Snapshot{DiskFree: 1300 * MiB, DiskTotal: 20 * GiB, MemoryAvailable: GiB, CPUQuota: 1}
	l := DefaultLimits()
	l.MinFree = GiB
	l.RequestedVolume = 512 * MiB
	if _, err := Plan(s, l); err == nil {
		t.Fatal("expected unsafe fixed-volume error")
	}
}

func TestCgroupMemoryAvailableIncludesInactiveFile(t *testing.T) {
	got := cgroupMemoryAvailable(2*GiB, 2*GiB-128*MiB, 1500*MiB)
	want := 1628 * MiB
	if got != want {
		t.Fatalf("available=%d, want %d", got, want)
	}
}

func TestCgroupMemoryAvailableClampsRacyStats(t *testing.T) {
	if got := cgroupMemoryAvailable(GiB, 128*MiB, 512*MiB); got != GiB {
		t.Fatalf("available=%d, want %d", got, GiB)
	}
}

func TestReadMemoryStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.stat")
	if err := os.WriteFile(path, []byte("anon 123\ninactive_file 456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readMemoryStat(path, "inactive_file")
	if err != nil {
		t.Fatal(err)
	}
	if got != 456 {
		t.Fatalf("inactive_file=%d, want 456", got)
	}
}
