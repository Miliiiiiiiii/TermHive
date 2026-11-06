package main

import "testing"

func TestParseCpuLine(t *testing.T) {
	counters, ok := parseCpuLine("cpu  100 0 50 800 30 0 0 20 0 0")
	if !ok {
		t.Fatal("aggregate cpu line not recognized")
	}
	// total = 100+50+800+30+20 = 1000; idle = 800+30 = 830
	if counters.total != 1000 || counters.idle != 830 {
		t.Fatalf("counters wrong: total=%d idle=%d", counters.total, counters.idle)
	}
	if _, ok := parseCpuLine("cpu0 1 2 3 4 5"); ok {
		t.Fatal("per-core line must not be treated as the aggregate")
	}
	if _, ok := parseCpuLine("meminfo: nope"); ok {
		t.Fatal("garbage line accepted")
	}
}

func TestParseMemKiB(t *testing.T) {
	total, used, ok := parseMemKiB("MemTotal:       8000000 kB\nMemFree:         1000000 kB\nMemAvailable:    4000000 kB\n")
	if !ok || total != 8000000 || used != 4000000 {
		t.Fatalf("meminfo parse wrong: total=%d used=%d ok=%v", total, used, ok)
	}
	// fallback to MemFree when MemAvailable is absent (old kernels)
	_, usedFallback, okFallback := parseMemKiB("MemTotal: 100 kB\nMemFree: 30 kB\n")
	if !okFallback || usedFallback != 70 {
		t.Fatalf("MemFree fallback wrong: used=%d ok=%v", usedFallback, okFallback)
	}
	if _, _, bad := parseMemKiB("nothing: here"); bad {
		t.Fatal("empty meminfo accepted")
	}
}

func TestParseDfKb(t *testing.T) {
	disks := parseDfKb("/dev/sda1 41251136 12345678 26837790  32% /\ntmpfs 4096000 0 4096000 0% /dev/shm\n/dev/sdb1 100 100 0 100% /data")
	if len(disks) != 3 {
		t.Fatalf("want 3 mounts, got %d", len(disks))
	}
	if disks[0]["mount"] != "/" || disks[0]["usePct"] != 29 || disks[0]["totalKb"] != int64(41251136) {
		t.Fatalf("root mount parsed wrong: %v", disks[0])
	}
	if disks[2]["usePct"] != 100 {
		t.Fatalf("full disk pct wrong: %v", disks[2]["usePct"])
	}
	if got := parseDfKb("Filesystem 1024-blocks Used Available Capacity Mounted on"); len(got) != 0 {
		t.Fatal("header line must be skipped")
	}
}
