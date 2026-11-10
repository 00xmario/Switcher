package sysload

import "math"

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
