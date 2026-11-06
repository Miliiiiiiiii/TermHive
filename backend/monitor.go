package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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
	// one round trip for everything: /proc + df + top processes + (optional)
	// nvidia-smi; segments split on bare === lines. nvidia-smi failures are
	// silenced server-side (no GPU → empty segment).
	raw, err := conn.runCapture(
		"cat /proc/stat /proc/meminfo /proc/net/dev; echo ===; df -kP 2>/dev/null | tail -n +2; echo ===; ps aux --sort=-pcpu 2>/dev/null | head -n 17; echo ===; nvidia-smi --query-gpu=name,utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits 2>/dev/null")
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32002, fmt.Sprintf("采样失败: %v", err))
	}
	segments := strings.Split(raw, "===\n")
	part := func(i int) string {
		if i >= len(segments) {
			return ""
		}
		return strings.TrimSpace(segments[i])
	}
	statPart := part(0)
	dfPart := part(1)
	psPart := part(2)
	gpuPart := part(3)
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
	rxKbps, txKbps := conn.netRateKbps(statPart)
	return map[string]any{
		"os":        "linux",
		"cpu":       cpu,
		"memTotal":  memTotal,
		"memUsed":   memUsed,
		"disks":     parseDfKb(dfPart),
		"netRxKbps": rxKbps,
		"netTxKbps": txKbps,
		"gpus":      parseGpuCsv(gpuPart),
		"procs":     parsePsAux(psPart),
	}, nil
}

// netCounters is one reading of an interface's /proc/net/dev byte counters.
type netCounters struct {
	rx int64
	tx int64
}

// netRateKbps diffs /proc/net/dev against the previous sample kept on the
// Conn (all interfaces except loopback summed), returning KB/s rates. The
// first sample reports 0.
func (conn *Conn) netRateKbps(statPart string) (float64, float64) {
	current := map[string]netCounters{}
	for _, line := range strings.Split(statPart, "\n") {
		at := strings.Index(line, ":")
		if at <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:at])
		if name == "lo" {
			continue
		}
		fields := strings.Fields(line[at+1:])
		if len(fields) < 9 {
			continue
		}
		rx, errRx := strconv.ParseInt(fields[0], 10, 64)
		tx, errTx := strconv.ParseInt(fields[8], 10, 64)
		if errRx != nil || errTx != nil {
			continue
		}
		current[name] = netCounters{rx: rx, tx: tx}
	}
	now := time.Now()
	conn.monitorMutex.Lock()
	previous, previousAt := conn.lastNet, conn.lastNetAt
	conn.lastNet, conn.lastNetAt = current, now
	conn.monitorMutex.Unlock()
	if previousAt.IsZero() || len(previous) == 0 {
		return 0, 0
	}
	seconds := now.Sub(previousAt).Seconds()
	if seconds <= 0 {
		return 0, 0
	}
	var rxDelta, txDelta int64
	for name, counters := range current {
		before, ok := previous[name]
		if !ok || counters.rx < before.rx || counters.tx < before.tx {
			continue // new interface or counter reset
		}
		rxDelta += counters.rx - before.rx
		txDelta += counters.tx - before.tx
	}
	return float64(rxDelta) / seconds / 1024, float64(txDelta) / seconds / 1024
}

// parsePsAux reads `ps aux --sort=-pcpu` output (header skipped by the server
// command) into compact process rows: user, pid, cpu%, mem%, command.
func parsePsAux(body string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 11 || fields[0] == "USER" || fields[0] == "USER%" {
			continue
		}
		pid, errPid := strconv.Atoi(fields[1])
		if errPid != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		mem, _ := strconv.ParseFloat(fields[3], 64)
		command := strings.Join(fields[10:], " ")
		if len(command) > 120 {
			command = command[:120] + "…"
		}
		out = append(out, map[string]any{
			"user":    fields[0],
			"pid":     pid,
			"cpuPct":  cpu,
			"memPct":  mem,
			"command": command,
		})
		if len(out) >= 16 {
			break
		}
	}
	return out
}

// parseGpuCsv reads nvidia-smi csv rows (name, util%, memUsedMiB, memTotalMiB).
func parseGpuCsv(body string) []map[string]any {
	out := []map[string]any{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 4 {
			continue
		}
		util, _ := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		memUsed, _ := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		memTotal, _ := strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		out = append(out, map[string]any{
			"name":       strings.TrimSpace(fields[0]),
			"utilPct":    util,
			"memUsedMb":  memUsed,
			"memTotalMb": memTotal,
		})
	}
	return out
}

// MonitorKill terminates a process the user selected in the dashboard — the
// same act as typing `kill` at the prompt, just triggered from the panel.
func (manager *Manager) MonitorKill(connectionID string, pid int, force bool, emitter *dbxpluginsdk.Emitter) *dbxpluginsdk.PluginError {
	conn, err := manager.connExact(connectionID, emitter)
	if err != nil {
		return dbxpluginsdk.NewError(-32001, err.Error())
	}
	signal := "-15"
	if force {
		signal = "-9"
	}
	if _, err := conn.runCapture(fmt.Sprintf("kill %s %d", signal, pid)); err != nil {
		return dbxpluginsdk.NewError(-32002, fmt.Sprintf("kill %d 失败: %v", pid, err))
	}
	manager.writeDiag(fmt.Sprintf("monitor kill %s pid=%d force=%v", conn.Spec.Host, pid, force))
	return nil
}
