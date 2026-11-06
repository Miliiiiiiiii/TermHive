package main

import (
	"fmt"
	"strconv"
	"strings"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// monitor.go — thin server health sampling (CPU / memory / disk mounts) for
// the dashboard. One exec round trip per sample over the shared connection.
// CPU is a percentage against the previous sample's counters kept on the Conn
// (the UI polls every few seconds, so the delta window is the poll interval;
// the first sample has no baseline and reports -1). Windows targets report
// os:"unsupported" — /proc is where every number here comes from.

// cpuCounters is one reading of /proc/stat's aggregate "cpu" line.
type cpuCounters struct {
	total int64
	idle  int64
}

func parseCpuLine(line string) (cpuCounters, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuCounters{}, false
	}
	var counters cpuCounters
	for index, field := range fields[1:] {
		value, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return cpuCounters{}, false
		}
		counters.total += value
		if index == 3 || index == 4 { // idle + iowait
			counters.idle += value
		}
	}
	return counters, true
}

// parseMemKiB pulls MemTotal and MemAvailable (fallback MemFree) in KiB.
func parseMemKiB(meminfo string) (total, used int64, ok bool) {
	available := int64(-1)
	for _, line := range strings.Split(meminfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value
		case "MemAvailable:":
			available = value
		case "MemFree:":
			if available < 0 {
				available = value
			}
		}
	}
	if total <= 0 || available < 0 {
		return 0, 0, false
	}
	return total, total - available, true
}

// parseDfKb reads POSIX `df -kP` body lines into mount entries (capped at 8).
func parseDfKb(dfBody string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(dfBody, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		totalKb, errTotal := strconv.ParseInt(fields[1], 10, 64)
		usedKb, errUsed := strconv.ParseInt(fields[2], 10, 64)
		if errTotal != nil || errUsed != nil {
			continue
		}
		usePct := 0
		if totalKb > 0 {
			usePct = int(float64(usedKb) / float64(totalKb) * 100)
		}
		out = append(out, map[string]any{
			"mount":   fields[len(fields)-1],
			"totalKb": totalKb,
			"usedKb":  usedKb,
			"usePct":  usePct,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// SampleMonitor collects one health snapshot for the connection. The shell
// kind gates POSIX-only collection (cmd/PowerShell targets answer
// unsupported); the CPU delta baseline lives on the Conn.
func (manager *Manager) SampleMonitor(connectionID string, emitter *dbxpluginsdk.Emitter) (map[string]any, *dbxpluginsdk.PluginError) {
	conn, err := manager.connExact(connectionID, emitter)
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32001, err.Error())
	}
	kind := conn.detectShellKind()
	if kind == kindCmd || kind == kindPowerShell {
		return map[string]any{"os": "unsupported"}, nil
	}
	raw, err := conn.runCapture("cat /proc/stat /proc/meminfo; echo ===; df -kP 2>/dev/null | tail -n +2")
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32002, fmt.Sprintf("采样失败: %v", err))
	}
	statPart := raw
	dfPart := ""
	if at := strings.Index(raw, "===\n"); at != -1 {
		statPart = raw[:at]
		dfPart = strings.TrimSpace(raw[at+len("===\n"):])
	}
	cpu := float64(-1)
	var counters cpuCounters
	var haveCounters bool
	memTotal, memUsed, _ := parseMemKiB(statPart)
	for _, line := range strings.Split(statPart, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "cpu ") {
			counters, haveCounters = parseCpuLine(line)
		}
	}
	if haveCounters {
		conn.monitorMutex.Lock()
		previous := conn.lastCpu
		conn.lastCpu = counters
		conn.monitorMutex.Unlock()
		if previous.total > 0 && counters.total > previous.total {
			idleDelta := counters.idle - previous.idle
			totalDelta := counters.total - previous.total
			if idleDelta >= 0 && totalDelta > 0 {
				cpu = float64(totalDelta-idleDelta) / float64(totalDelta) * 100
			}
		}
	}
	return map[string]any{
		"os":       "linux",
		"cpu":      cpu,
		"memTotal": memTotal,
		"memUsed":  memUsed,
		"disks":    parseDfKb(dfPart),
	}, nil
}
