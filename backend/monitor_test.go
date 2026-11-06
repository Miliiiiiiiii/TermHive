package main

import (
	"strings"
	"testing"
	"time"
)

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

func TestParsePsAux(t *testing.T) {
	body := "USER       PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND\n" +
		"root         1  0.0  0.1 125432  3120 ?        Ss   Oct01   0:31 /usr/lib/systemd/systemd --switched-root\n" +
		"root      4811 43.2 12.8 8132556 1048576 ?        Sl   Oct01 331:11 java -jar finpss-ccs-booter.jar\n" +
		"cc50      5233  0.1  0.3 118220  5120 pts/0    R+   10:31   0:00 ps aux --sort=-pcpu"
	procs := parsePsAux(body)
	if len(procs) != 3 {
		t.Fatalf("want 3 processes (header skipped), got %d", len(procs))
	}
	java := procs[1]
	if java["pid"] != 4811 || java["cpuPct"].(float64) != 43.2 {
		t.Fatalf("java row parsed wrong: %v", java)
	}
	if command := java["command"].(string); !strings.HasPrefix(command, "java -jar") {
		t.Fatalf("command should rejoin the tail fields: %q", command)
	}
}

func TestParseGpuCsv(t *testing.T) {
	gpus := parseGpuCsv("NVIDIA GeForce RTX 3090, 78, 22012, 24576\nTesla V100-SXM2-32GB, 12, 1024, 32768")
	if len(gpus) != 2 {
		t.Fatalf("want 2 gpus, got %d", len(gpus))
	}
	first := gpus[0]
	if first["name"] != "NVIDIA GeForce RTX 3090" || first["utilPct"].(float64) != 78 || first["memTotalMb"].(float64) != 24576 {
		t.Fatalf("gpu row parsed wrong: %v", first)
	}
	if len(parseGpuCsv("")) != 0 {
		t.Fatal("empty gpu segment must parse to an empty list")
	}
}

func TestNetRateKbpsDiff(t *testing.T) {
	conn := &Conn{}
	first := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		"  eth0: 1048576 0 0 0 0 0 0 0  2097152 0 0 0 0 0 0 0\n" +
		"    lo: 100 0 0 0 0 0 0 0  100 0 0 0 0 0 0 0\n"
	rx, tx := conn.netRateKbps(first)
	if rx != 0 || tx != 0 {
		t.Fatal("first sample must report zero rates")
	}
	conn.monitorMutex.Lock()
	conn.lastNetAt = conn.lastNetAt.Add(-2 * time.Second) // pretend 2s elapsed
	conn.monitorMutex.Unlock()
	second := strings.Replace(first, "eth0: 1048576", "eth0: 2097152", 1) // +1 MiB rx
	rx, tx = conn.netRateKbps(second)
	// 1 MiB over 2s = 512 KB/s; tx unchanged → 0
	if rx < 511 || rx > 513 || tx != 0 {
		t.Fatalf("rates wrong: rx=%.1f tx=%.1f", rx, tx)
	}
}
