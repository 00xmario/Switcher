package sysload

import (
	"math"
	"strings"
	"unicode/utf8"
)

// View is a reading as GET /api/system answers it, for this computer or, on
// a Mac that uses another Switcher's accounts, for that host.
type View struct {
	Machine   string `json:"machine"`                // the computer's name
	Remote    bool   `json:"remote"`                 // the paired host's load, not this computer's
	Platform  string `json:"platform,omitempty"`     // "darwin" or "linux"
	Available bool   `json:"available"`              // false: no reading (unsupported, unreachable, an older host)
	Reason    string `json:"reason,omitempty"`       // why not: "unsupported", "unreachable" or "update"
	CPU       int    `json:"cpu"`                    // percent of all cores
	Cores     int    `json:"cores,omitempty"`        //
	Memory    int    `json:"memory"`                 // percent in use
	MemUsed   uint64 `json:"memory_used,omitempty"`  // bytes
	MemTotal  uint64 `json:"memory_total,omitempty"` // bytes
	GPU       *int   `json:"gpu,omitempty"`          // percent; absent without a reading
}

// ViewOf turns a reading into the API's view.
func ViewOf(s Sample, machine, platform string) View {
	v := View{Machine: machine, Platform: platform, Available: true, CPU: percent(s.CPU), Cores: s.Cores,
		Memory: percent(s.Memory()), MemUsed: s.MemUsed, MemTotal: s.MemTotal}
	if s.HasGPU {
		gpu := percent(s.GPU)
		v.GPU = &gpu
	}
	return v
}

// maxMemory bounds a reported memory size (64 TiB), so a host's answer
// cannot put nonsense on screen.
const maxMemory = 64 << 40

// Sanitize keeps a view another computer sent within what a reading can be,
// and drops its strings: the caller names the machine itself.
func (v *View) Sanitize() {
	v.Machine, v.Reason = "", ""
	if v.Platform != "darwin" && v.Platform != "linux" {
		v.Platform = ""
	}
	v.CPU, v.Memory = bound(v.CPU), bound(v.Memory)
	if v.Cores < 0 || v.Cores > 4096 {
		v.Cores = 0
	}
	if v.MemTotal > maxMemory || v.MemUsed > v.MemTotal {
		v.MemUsed, v.MemTotal = 0, 0
	}
	if v.GPU != nil {
		gpu := bound(*v.GPU)
		v.GPU = &gpu
	}
	if !v.Available {
		v.CPU, v.Memory, v.Cores, v.MemUsed, v.MemTotal, v.GPU = 0, 0, 0, 0, 0, nil
	}
}

func percent(v float64) int { return int(math.Round(clamp(v))) }

func bound(v int) int { return max(0, min(100, v)) }

// AppsView is GET /api/system/apps: the load, and what is using it by
// application.
type AppsView struct {
	View
	Apps      []App `json:"apps"`
	Processes int   `json:"processes"`
	GPUApps   bool  `json:"gpu_apps"`
}

// maxApps bounds the applications another computer may list.
const maxApps = 80

// Sanitize keeps a breakdown another computer sent within what one can be:
// names as plain, short text and numbers in range.
func (v *AppsView) Sanitize() {
	v.View.Sanitize()
	if len(v.Apps) > maxApps {
		v.Apps = v.Apps[:maxApps]
	}
	if v.Processes < 0 || v.Processes > 1<<22 {
		v.Processes = 0
	}
	kept := v.Apps[:0]
	for _, a := range v.Apps {
		a.Name = plainName(a.Name)
		if a.Name == "" {
			continue
		}
		a.Processes = max(0, min(a.Processes, 1<<22))
		a.CPU = clamp(a.CPU)
		if a.Memory > maxMemory {
			a.Memory = 0
		}
		if a.GPUMemory > maxMemory {
			a.GPUMemory = 0
		}
		if a.GPU != nil {
			g := clamp(*a.GPU)
			a.GPU = &g
		}
		if !v.GPUApps {
			a.GPU, a.GPUMemory = nil, 0
		}
		kept = append(kept, a)
	}
	v.Apps = kept
	if !v.Available {
		v.Apps, v.Processes, v.GPUApps = nil, 0, false
	}
}

// plainName keeps a name printable and short: no control characters or
// marks that reorder text, at most 64 characters.
func plainName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > 64 {
		s = string([]rune(s)[:64])
	}
	return strings.TrimSpace(s)
}
