package sysload

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linux keeps each process's processor time and memory in /proc, and the
// graphics drivers that support it (amdgpu, i915, xe, nouveau, msm and
// others) report each client's busy time per engine in the fdinfo of its
// /dev/dri file.

// userHZ is the unit of /proc's processor times on every Linux.
const userHZ = 100

// pfKthread marks a kernel thread in /proc/<pid>/stat's flags.
const pfKthread = 0x00200000

// drmRescan is how often a process's open files are listed again to find
// its graphics clients; between listings only their fdinfo is read.
const drmRescan = 10 * time.Second

type linuxProcs struct {
	root string
	page uint64

	mu  sync.Mutex
	drm map[int]drmFiles
}

type drmFiles struct {
	start     uint64
	fds       []string
	scannedAt time.Time
}

func newLinuxProcs(root string) *linuxProcs {
	return &linuxProcs{root: root, page: uint64(os.Getpagesize()), drm: map[int]drmFiles{}}
}

func (l *linuxProcs) read() (procSnapshot, error) {
	entries, err := os.ReadDir(l.root)
	if err != nil {
		return procSnapshot{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var snap procSnapshot
	seen := map[string]bool{}
	alive := map[int]bool{}
	now := time.Now()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(l.root, e.Name())
		p, ok := l.process(pid, dir)
		if !ok {
			continue
		}
		alive[pid] = true
		snap.procs = append(snap.procs, p)
		for _, c := range l.clients(pid, p.start, dir, now) {
			if c.ok {
				snap.gpuOK = true
			}
			if c.key == "" || seen[c.key] {
				continue // a client shared between processes counts once
			}
			seen[c.key] = true
			snap.gpu = append(snap.gpu, c.gpuClient)
		}
	}
	for pid := range l.drm {
		if !alive[pid] {
			delete(l.drm, pid)
		}
	}
	completeNames(snap.procs)
	return snap, nil
}

// process reads one process's name, start, processor time and memory.
func (l *linuxProcs) process(pid int, dir string) (procInfo, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return procInfo{}, false
	}
	comm, fields, ok := parsePidStat(raw)
	if !ok {
		return procInfo{}, false
	}
	p := procInfo{pid: pid, start: fields.start, cpu: time.Duration(fields.utime+fields.stime) * time.Second / userHZ}
	if statm, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
		if f := strings.Fields(string(statm)); len(f) > 1 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				p.mem = pages * l.page
			}
		}
	}
	switch {
	case fields.flags&pfKthread != 0:
		p.name = "Linux kernel"
	default:
		// Named after the program file, never the name a process gives
		// itself, which can hold its arguments (npm sets it to the command
		// it runs). Another user's program file is not ours to read: then
		// the kernel's name for it, cut at 15 characters.
		if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
			p.name = programName(exe)
		} else {
			p.name = kernelName(comm)
			p.cut = p.name == comm && len(comm) >= 15
		}
	}
	return p, p.name != ""
}

// kernelName is the kernel's name for another user's program, which the
// process may have set itself (node copies its title there): only its first
// word, and none that looks like a setting or an address.
func kernelName(comm string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(comm), " ")
	if name == "" || strings.ContainsAny(name, "=:@/\\") {
		return "Other programs"
	}
	return name
}

// programName names a program after its file. A file named after its
// version (claude/versions/2.1.318) is named after the folder that holds
// its versions.
func programName(exe string) string {
	base := appName(exe)
	if versionLike(base) {
		dir := filepath.Dir(strings.TrimSuffix(exe, " (deleted)"))
		for dir != "/" && dir != "." && (filepath.Base(dir) == "versions" || filepath.Base(dir) == "bin" || versionLike(filepath.Base(dir))) {
			dir = filepath.Dir(dir)
		}
		if name := filepath.Base(dir); dir != "/" && dir != "." && name != "" {
			base = name
		}
	}
	return base
}

// completeNames gives a name the kernel cut at 15 characters the full name
// of the one program seen that starts with it, and the names people know.
func completeNames(procs []procInfo) {
	full := map[string]bool{}
	for _, p := range procs {
		if !p.cut {
			full[p.name] = true
		}
	}
	for i, p := range procs {
		if p.cut {
			match := ""
			for name := range full {
				if strings.HasPrefix(name, p.name) {
					if match != "" {
						match = ""
						break
					}
					match = name
				}
			}
			if match != "" {
				procs[i].name = match
			}
		}
		procs[i].name = friendlyName(procs[i].name)
	}
}

// versionLike says a name is a version number, such as 2.1.318.
func versionLike(name string) bool {
	if name == "" || !strings.Contains(name, ".") {
		return false
	}
	for _, r := range name {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

type pidStat struct {
	flags, utime, stime, start uint64
}

// parsePidStat reads /proc/<pid>/stat: the name sits in parentheses and may
// hold spaces and parentheses itself, so the fields count from the last ")".
func parsePidStat(raw []byte) (string, pidStat, bool) {
	open, close := bytes.IndexByte(raw, '('), bytes.LastIndexByte(raw, ')')
	if open < 0 || close < open {
		return "", pidStat{}, false
	}
	comm := string(raw[open+1 : close])
	f := strings.Fields(string(raw[close+1:]))
	// f[0] is field 3 (state): flags is field 9, utime 14, stime 15,
	// starttime 22.
	if len(f) < 20 {
		return "", pidStat{}, false
	}
	var st pidStat
	for _, x := range []struct {
		i   int
		out *uint64
	}{{6, &st.flags}, {11, &st.utime}, {12, &st.stime}, {19, &st.start}} {
		n, err := strconv.ParseUint(f[x.i], 10, 64)
		if err != nil {
			return "", pidStat{}, false
		}
		*x.out = n
	}
	return comm, st, true
}

type drmClient struct {
	gpuClient
	ok bool // the driver reports busy time
}

// clients reads a process's graphics clients from the fdinfo of its
// /dev/dri files, listing its files again now and then.
func (l *linuxProcs) clients(pid int, start uint64, dir string, now time.Time) []drmClient {
	known, ok := l.drm[pid]
	if !ok || known.start != start || now.Sub(known.scannedAt) > drmRescan {
		known = drmFiles{start: start, scannedAt: now}
		if fds, err := os.ReadDir(filepath.Join(dir, "fd")); err == nil {
			for _, fd := range fds {
				if target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name())); err == nil && strings.HasPrefix(target, "/dev/dri/") {
					known.fds = append(known.fds, fd.Name())
				}
			}
		}
		l.drm[pid] = known
	}
	var out []drmClient
	for _, fd := range known.fds {
		raw, err := os.ReadFile(filepath.Join(dir, "fdinfo", fd))
		if err != nil {
			continue
		}
		if c, ok := parseDRMFdinfo(raw); ok {
			c.pid = pid
			out = append(out, c)
		}
	}
	return out
}

// parseDRMFdinfo reads one DRM file's fdinfo: the client id and device, each
// engine's busy time (nanoseconds, or cycles out of the engine's total, as
// xe counts it), and the graphics memory it holds (amdgpu's vram, Intel's
// local and vram regions).
func parseDRMFdinfo(raw []byte) (drmClient, bool) {
	var c drmClient
	var id, dev string
	var legacy uint64
	c.busy = map[string]time.Duration{}
	c.cycles = map[string][2]uint64{}
	c.capacity = map[string]uint64{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case key == "drm-client-id":
			id = value
		case key == "drm-pdev":
			dev = value
		case strings.HasPrefix(key, "drm-engine-capacity-"):
			if v, err := strconv.ParseUint(value, 10, 64); err == nil && v > 1 && v <= 64 {
				c.capacity[strings.TrimPrefix(key, "drm-engine-capacity-")] = v
			}
		case strings.HasPrefix(key, "drm-engine-"):
			n, unit, _ := strings.Cut(value, " ")
			if unit != "ns" {
				continue
			}
			if v, err := strconv.ParseUint(n, 10, 64); err == nil {
				c.busy[strings.TrimPrefix(key, "drm-engine-")] = time.Duration(v)
				c.ok = true
			}
		case strings.HasPrefix(key, "drm-total-cycles-"), strings.HasPrefix(key, "drm-cycles-"):
			total := strings.HasPrefix(key, "drm-total-cycles-")
			engine := strings.TrimPrefix(strings.TrimPrefix(key, "drm-total-cycles-"), "drm-cycles-")
			if v, err := strconv.ParseUint(value, 10, 64); err == nil {
				pair := c.cycles[engine]
				if total {
					pair[1] = v
				} else {
					pair[0] = v
				}
				c.cycles[engine] = pair
				c.ok = true
			}
		case key == "drm-memory-vram":
			legacy = drmBytes(value)
		case strings.HasPrefix(key, "drm-resident-vram"), strings.HasPrefix(key, "drm-resident-local"):
			c.vram += drmBytes(value)
		}
	}
	if id == "" {
		return drmClient{}, false
	}
	if c.vram == 0 {
		c.vram = legacy // amdgpu before drm-resident-*
	}
	c.key = dev + "/" + id
	return c, true
}

// drmBytes reads a size such as "11064 KiB" or "6 MiB".
func drmBytes(value string) uint64 {
	n, unit, _ := strings.Cut(value, " ")
	v, err := strconv.ParseUint(n, 10, 64)
	if err != nil {
		return 0
	}
	switch unit {
	case "KiB":
		return v << 10
	case "MiB":
		return v << 20
	case "GiB":
		return v << 30
	case "":
		return v
	}
	return 0
}
