package sysload

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// parsePS reads ps's pid, processor time, resident kilobytes and path.
func parsePS(out []byte) procSnapshot {
	var snap procSnapshot
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		f := strings.Fields(scanner.Text())
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		cpu, ok := psTime(f[1])
		rss, err2 := strconv.ParseUint(f[2], 10, 64)
		if err1 != nil || err2 != nil || !ok || pid <= 0 {
			continue
		}
		name := friendlyName(appName(strings.Join(f[3:], " ")))
		if name == "" {
			continue
		}
		snap.procs = append(snap.procs, procInfo{pid: pid, name: name, cpu: cpu, mem: rss << 10})
	}
	return snap
}

// psTime reads ps's processor time: [[dd-]hh:]mm:ss.ss.
func psTime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	total := seconds
	for i, unit := len(parts)-2, 60.0; i >= 0; i, unit = i-1, unit*60 {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, false
		}
		total += float64(n) * unit
	}
	total += float64(days) * 86400
	return time.Duration(total * float64(time.Second)), true
}
