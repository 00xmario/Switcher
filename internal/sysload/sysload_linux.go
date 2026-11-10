package sysload

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func platform() reader {
	return reader{
		cpu:    func() (cpuTimes, error) { return procStat("/proc/stat") },
		memory: func() (uint64, uint64, error) { return procMeminfo("/proc/meminfo") },
		gpu:    linuxGPU(sysfsGPU("/sys/class/drm"), sleepingNvidia("/sys/class/drm", nvidiaSMI())),
		cores:  runtime.NumCPU,
		procs:  newLinuxProcs("/proc").read,
	}
}

// procStat reads the processor ticks from the first line of /proc/stat:
// user nice system idle iowait irq softirq steal (guest time is already in
// user). Waiting on disk counts as idle.
func procStat(path string) (cpuTimes, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadString('\n')
	if err != nil && line == "" {
		return cpuTimes{}, err
	}
	return parseProcStat(line)
}

func parseProcStat(line string) (cpuTimes, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuTimes{}, errors.New("unexpected /proc/stat")
	}
	var v [8]uint64
	for i := 0; i < len(v) && i+1 < len(fields); i++ {
		n, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuTimes{}, errors.New("unexpected /proc/stat")
		}
		v[i] = n
	}
	idle := v[3] + v[4]
	busy := v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
	return cpuTimes{busy: busy, total: busy + idle}, nil
}

// procMeminfo reads memory in use as total less available, the share the
// kernel says new programs cannot have without swapping.
func procMeminfo(path string) (used, total uint64, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	return parseMeminfo(raw)
}

func parseMeminfo(raw []byte) (used, total uint64, err error) {
	var available uint64
	var haveTotal, haveAvailable bool
	for _, line := range bytes.Split(raw, []byte("\n")) {
		key, rest, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			continue
		}
		fields := strings.Fields(string(rest))
		if len(fields) == 0 {
			continue
		}
		kb, perr := strconv.ParseUint(fields[0], 10, 64)
		if perr != nil {
			continue
		}
		switch string(key) {
		case "MemTotal":
			total, haveTotal = kb*1024, true
		case "MemAvailable":
			available, haveAvailable = kb*1024, true
		}
	}
	if !haveTotal || !haveAvailable || total == 0 {
		return 0, 0, errors.New("unexpected /proc/meminfo")
	}
	return total - min(available, total), total, nil
}

// cardName matches a graphics card's own directory (card0), not its
// connectors (card0-HDMI-A-1).
var cardName = regexp.MustCompile(`^card[0-9]+$`)

// suspended reports a card the kernel has powered down: it is idle, and
// asking it anything could wake it.
func suspended(device string) bool {
	raw, err := os.ReadFile(filepath.Join(device, "power", "runtime_status"))
	return err == nil && strings.TrimSpace(string(raw)) == "suspended"
}

// sysfsGPU reads the busy share AMD's driver (and others that follow it)
// keeps for each card, and answers the busiest. A suspended card counts as
// idle and is not asked.
func sysfsGPU(root string) func() (float64, bool) {
	return func() (float64, bool) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return 0, false
		}
		best, found := 0.0, false
		for _, e := range entries {
			if !cardName.MatchString(e.Name()) {
				continue
			}
			device := filepath.Join(root, e.Name(), "device")
			if _, err := os.Stat(filepath.Join(device, "gpu_busy_percent")); err == nil && suspended(device) {
				found = true
				continue
			}
			raw, err := os.ReadFile(filepath.Join(device, "gpu_busy_percent"))
			if err != nil {
				continue
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
			if err != nil {
				continue
			}
			best, found = max(best, clamp(v)), true
		}
		return best, found
	}
}

// nvidiaAsleep reports that every NVIDIA card the kernel lists is powered
// down, as on a laptop running on its other graphics: nvidia-smi would wake
// one, so the reading is idle instead.
func nvidiaAsleep(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	seen := false
	for _, e := range entries {
		if !cardName.MatchString(e.Name()) {
			continue
		}
		device := filepath.Join(root, e.Name(), "device")
		raw, err := os.ReadFile(filepath.Join(device, "vendor"))
		if err != nil || strings.TrimSpace(string(raw)) != "0x10de" {
			continue
		}
		if !suspended(device) {
			return false
		}
		seen = true
	}
	return seen
}

// sleepingNvidia answers idle for NVIDIA cards that are all powered down,
// and asks nvidia-smi otherwise.
func sleepingNvidia(root string, nvidia func(context.Context) (float64, bool)) func(context.Context) (float64, bool) {
	return func(ctx context.Context) (float64, bool) {
		if nvidiaAsleep(root) {
			return 0, true
		}
		return nvidia(ctx)
	}
}

// nvidiaSMI asks NVIDIA's own tool for each card's utilization, when it is
// installed. It is looked up once, at an absolute path.
func nvidiaSMI() func(context.Context) (float64, bool) {
	var once sync.Once
	var path string
	return func(ctx context.Context) (float64, bool) {
		once.Do(func() {
			if p, err := exec.LookPath("nvidia-smi"); err == nil && filepath.IsAbs(p) {
				path = p
			}
		})
		if path == "" {
			return 0, false
		}
		cmd := exec.CommandContext(ctx, path, "--query-gpu=utilization.gpu", "--format=csv,noheader,nounits")
		cmd.WaitDelay = 500 * time.Millisecond // a tool that ignores the kill does not hold the reading
		out, err := cmd.Output()
		if err != nil {
			return 0, false
		}
		return parseNvidia(out)
	}
}

func parseNvidia(out []byte) (float64, bool) {
	best, found := 0.0, false
	for _, line := range strings.Split(string(out), "\n") {
		v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
		if err != nil {
			continue
		}
		best, found = max(best, clamp(v)), true
	}
	return best, found
}

// linuxGPU prefers the driver's sysfs counter, then NVIDIA's tool. A card
// neither knows (most Intel graphics) shows as having no reading.
func linuxGPU(sysfs func() (float64, bool), nvidia func(context.Context) (float64, bool)) func(context.Context) (float64, bool) {
	return func(ctx context.Context) (float64, bool) {
		if v, ok := sysfs(); ok {
			return v, true
		}
		return nvidia(ctx)
	}
}
