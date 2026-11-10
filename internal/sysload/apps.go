package sysload

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// App is one application's share of the computer: its processes together,
// named after the program, never its arguments or files.
type App struct {
	Name      string   `json:"name"`
	Processes int      `json:"processes"`
	CPU       float64  `json:"cpu"`                  // percent of all cores
	Memory    uint64   `json:"memory"`               // bytes resident
	GPU       *float64 `json:"gpu,omitempty"`        // percent of the graphics processor's time
	GPUMemory uint64   `json:"gpu_memory,omitempty"` // bytes of graphics memory
}

// Apps is what is using the computer, by application.
type Apps struct {
	Apps      []App `json:"apps"`
	Processes int   `json:"processes"` // all processes running
	// GPUApps says this computer reports graphics use per application.
	GPUApps bool `json:"gpu_apps"`
}

// procInfo is one process at one moment.
type procInfo struct {
	pid   int
	start uint64 // when it started, to tell a reused pid from the old one
	name  string // the application it belongs to
	cpu   time.Duration
	mem   uint64
	cut   bool // the name is the kernel's, cut at 15 characters
}

// gpuClient is one graphics client of a process: its busy time per engine,
// cumulative, and the graphics memory it holds.
type gpuClient struct {
	key  string // the device and the driver's client id
	pid  int
	busy map[string]time.Duration
	// cycles are an engine's busy cycles and its total cycles, as xe
	// counts them instead of time.
	cycles map[string][2]uint64
	vram   uint64
}

type procSnapshot struct {
	procs []procInfo
	gpu   []gpuClient
	gpuOK bool // the driver reports busy time per client
}

const (
	// appsFresh is how long a breakdown answers repeated requests.
	appsFresh = 1500 * time.Millisecond
	// appsWindow is the shortest span shares are measured over.
	appsWindow = 500 * time.Millisecond
	// appsTop is how many applications lead each ranking.
	appsTop = 15
)

// Apps answers what is using the computer. The first breakdown after a
// quiet spell waits half a second to measure; later ones answer at once.
func (s *Sampler) Apps(ctx context.Context) (Apps, error) {
	if s == nil || s.r.procs == nil {
		return Apps{}, ErrUnsupported
	}
	s.appsMu.Lock()
	defer s.appsMu.Unlock()
	now := s.now()
	if !s.appsAt.IsZero() && now.Sub(s.appsAt) < appsFresh && now.After(s.appsAt) {
		return s.apps, nil
	}
	snap, err := s.r.procs()
	if err != nil {
		return Apps{}, err
	}
	if s.procsAt.IsZero() || now.Sub(s.procsAt) > stale || !now.After(s.procsAt) {
		s.procsPrev, s.procsAt = snap, now
		timer := time.NewTimer(appsWindow)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Apps{}, ctx.Err()
		case <-timer.C:
		}
		now = s.now()
		if snap, err = s.r.procs(); err != nil {
			return Apps{}, err
		}
	}
	cores := 1
	if s.r.cores != nil {
		cores = max(1, s.r.cores())
	}
	apps := breakdown(s.procsPrev, snap, now.Sub(s.procsAt), cores)
	s.procsPrev, s.procsAt = snap, now
	s.apps, s.appsAt = apps, now
	return apps, nil
}

// breakdown groups two snapshots' difference by application.
func breakdown(before, after procSnapshot, elapsed time.Duration, cores int) Apps {
	type key struct {
		pid   int
		start uint64
	}
	prev := make(map[key]time.Duration, len(before.procs))
	for _, p := range before.procs {
		prev[key{p.pid, p.start}] = p.cpu
	}
	byName := map[string]*App{}
	nameOf := make(map[int]string, len(after.procs))
	for _, p := range after.procs {
		app := byName[p.name]
		if app == nil {
			app = &App{Name: p.name}
			byName[p.name] = app
		}
		app.Processes++
		app.Memory += p.mem
		nameOf[p.pid] = p.name
		if was, ok := prev[key{p.pid, p.start}]; ok && p.cpu >= was && elapsed > 0 {
			app.CPU += 100 * float64(p.cpu-was) / (float64(elapsed) * float64(cores))
		}
	}
	if after.gpuOK {
		prevGPU := make(map[string]gpuClient, len(before.gpu))
		for _, c := range before.gpu {
			prevGPU[c.key] = c
		}
		for _, c := range after.gpu {
			app := byName[nameOf[c.pid]]
			if app == nil {
				continue
			}
			app.GPUMemory += c.vram
			share := 0.0
			if was, ok := prevGPU[c.key]; ok && elapsed > 0 {
				// A client's share is its busiest engine's.
				for engine, busy := range c.busy {
					if before, ok := was.busy[engine]; ok && busy >= before {
						share = max(share, 100*float64(busy-before)/float64(elapsed))
					}
				}
				for engine, now := range c.cycles {
					before, ok := was.cycles[engine]
					if ok && now[0] >= before[0] && now[1] > before[1] {
						share = max(share, 100*float64(now[0]-before[0])/float64(now[1]-before[1]))
					}
				}
			}
			gpu := share
			if app.GPU != nil {
				gpu += *app.GPU
			}
			gpu = min(gpu, 100)
			app.GPU = &gpu
		}
	}
	all := make([]App, 0, len(byName))
	for _, app := range byName {
		app.CPU = round1(min(app.CPU, 100))
		if app.GPU != nil {
			g := round1(*app.GPU)
			app.GPU = &g
		} else if after.gpuOK {
			zero := 0.0
			app.GPU = &zero
		}
		all = append(all, *app)
	}
	return Apps{Apps: leaders(all), Processes: len(after.procs), GPUApps: after.gpuOK}
}

// leaders keeps the applications that lead any ranking, by processor first.
func leaders(all []App) []App {
	// Each ranking keeps only apps that use some of it, so the list never
	// fills up with idle programs, and orders ties by name, so it is the
	// same from one reading to the next.
	keep := map[string]bool{}
	rank := func(of func(a App) float64) {
		sort.SliceStable(all, func(i, j int) bool {
			if x, y := of(all[i]), of(all[j]); x != y {
				return x > y
			}
			return all[i].Name < all[j].Name
		})
		for i := 0; i < len(all) && i < appsTop && of(all[i]) > 0; i++ {
			keep[all[i].Name] = true
		}
	}
	gpuOf := func(a App) float64 {
		if a.GPU == nil {
			return 0
		}
		return *a.GPU
	}
	rank(func(a App) float64 { return float64(a.GPUMemory) })
	rank(gpuOf)
	rank(func(a App) float64 { return float64(a.Memory) })
	rank(func(a App) float64 { return a.CPU })
	out := make([]App, 0, len(keep))
	for _, app := range all {
		if keep[app.Name] {
			out = append(out, app)
		}
	}
	return out
}

// friendlyNames are the names people know some programs by.
var friendlyNames = map[string]string{
	"claude": "Claude Code", "codex": "Codex", "claude-desktop": "Claude Desktop", "opencode": "OpenCode",
	"switcher": "Switcher", "switcher-tailnet": "Switcher", "chrome": "Chrome", "chrome-headless-shell": "Chrome (headless)",
	"chromium": "Chromium", "firefox": "Firefox", "gnome-shell": "GNOME Shell", "node": "Node.js", "bun": "Bun",
	"python3": "Python", "python": "Python", "Xorg": "X server", "Xwayland": "Xwayland", "dockerd": "Docker",
	"containerd": "Docker", "postgres": "PostgreSQL", "code": "VS Code",
}

func friendlyName(name string) string {
	if friendly, ok := friendlyNames[name]; ok {
		return friendly
	}
	if strings.HasPrefix(name, "python3.") {
		return "Python"
	}
	return name
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// appName names an executable's application: on macOS the outermost app
// bundle it lives in, so helpers count with their app; otherwise the
// program's file name.
func appName(path string) string {
	path = strings.TrimSuffix(strings.TrimSpace(path), " (deleted)")
	if i := strings.Index(path, ".app/"); i >= 0 {
		path = path[:i+4]
	}
	if i := strings.LastIndexByte(strings.TrimSuffix(path, "/"), '/'); i >= 0 {
		path = path[i+1:]
	}
	return strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".app")
}
