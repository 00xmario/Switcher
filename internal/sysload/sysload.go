// Package sysload measures how busy this computer is: processor, memory and
// graphics load, for the menu's load card. It reads what the system already
// keeps (/proc and sysfs on Linux, the Mach host statistics and the graphics
// driver's own counters on macOS), without cgo, and only while someone looks:
// a reading is taken on request and kept for a moment, so a menu left closed
// costs nothing.
package sysload

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// ErrUnsupported says this platform has no load readings.
var ErrUnsupported = errors.New("load readings are not available on this platform")

// Sample is one reading. Percentages run from 0 to 100.
type Sample struct {
	CPU      float64 // share of all cores busy since the previous reading
	Cores    int
	MemUsed  uint64 // bytes in use: what macOS's Activity Monitor and Linux's "available" call used
	MemTotal uint64
	GPU      float64 // busiest graphics processor
	HasGPU   bool    // false when the driver reports no utilization
}

// Memory is the share of memory in use.
func (s Sample) Memory() float64 {
	if s.MemTotal == 0 {
		return 0
	}
	return clamp(100 * float64(s.MemUsed) / float64(s.MemTotal))
}

// cpuTimes are cumulative processor ticks: busy and all.
type cpuTimes struct{ busy, total uint64 }

// reader is what a platform provides. Each call is cheap except gpu, which
// may start a short-lived tool (ioreg, nvidia-smi) and is read less often.
type reader struct {
	cpu    func() (cpuTimes, error)
	memory func() (used, total uint64, err error)
	gpu    func(ctx context.Context) (float64, bool)
	cores  func() int
}

const (
	// fresh is how long a reading answers repeated requests; a menu asks
	// every two seconds and several screens may ask at once.
	fresh = 900 * time.Millisecond
	// gpuFresh spaces out the graphics reading, which may start a process
	// (ioreg, nvidia-smi). After the first, it is taken in the background,
	// so a slow one never holds up a reading.
	gpuFresh = 5 * time.Second
	// window is the shortest span a processor share is measured over. Asked
	// with no recent reading, the sampler waits this long for a second one.
	window = 250 * time.Millisecond
	// stale is how old the previous processor ticks may be and still give
	// the share; older ones would show a long average, not the load now.
	stale = 10 * time.Second
	// gpuTimeout bounds the graphics tool.
	gpuTimeout = 1500 * time.Millisecond
)

// Sampler takes readings, one at a time, and keeps the last.
type Sampler struct {
	r   reader
	now func() time.Time

	mu      sync.Mutex
	prev    cpuTimes
	prevAt  time.Time
	last    Sample
	lastAt  time.Time
	gpu     float64
	hasGPU  bool
	gpuAt   time.Time
	gpuBusy bool // a background graphics reading is on its way
}

// New returns a sampler for this platform.
func New() *Sampler { return &Sampler{r: platform(), now: time.Now} }

// Supported reports whether this platform has load readings.
func (s *Sampler) Supported() bool { return s != nil && s.r.cpu != nil }

// Read returns a current reading. The first one after a quiet spell waits a
// quarter of a second to measure the processor; later ones answer at once.
func (s *Sampler) Read(ctx context.Context) (Sample, error) {
	if !s.Supported() {
		return Sample{}, ErrUnsupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if !s.lastAt.IsZero() && now.Sub(s.lastAt) < fresh && now.After(s.lastAt) {
		return s.last, nil
	}
	ticks, err := s.r.cpu()
	if err != nil {
		return Sample{}, err
	}
	if s.prevAt.IsZero() || now.Sub(s.prevAt) > stale || !now.After(s.prevAt) || ticks.total <= s.prev.total {
		s.prev, s.prevAt = ticks, now
		timer := time.NewTimer(window)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Sample{}, ctx.Err()
		case <-timer.C:
		}
		now = s.now()
		if ticks, err = s.r.cpu(); err != nil {
			return Sample{}, err
		}
	}
	sample := Sample{CPU: share(s.prev, ticks)}
	s.prev, s.prevAt = ticks, now
	if s.r.cores != nil {
		sample.Cores = s.r.cores()
	}
	if s.r.memory != nil {
		if used, total, err := s.r.memory(); err == nil && total > 0 {
			sample.MemUsed, sample.MemTotal = min(used, total), total
		}
	}
	if s.r.gpu != nil {
		switch {
		case s.gpuAt.IsZero() || !s.gpuBusy && now.Sub(s.gpuAt) > stale:
			// The first reading, and the first after a quiet spell, waits
			// for the graphics too, so the card shows neither a gap nor an
			// old value.
			gctx, cancel := context.WithTimeout(ctx, gpuTimeout)
			s.gpu, s.hasGPU = s.r.gpu(gctx)
			cancel()
			s.gpuAt = now
		case !s.gpuBusy && (now.Sub(s.gpuAt) >= gpuFresh || !now.After(s.gpuAt)):
			s.gpuBusy, s.gpuAt = true, now
			go s.readGPU()
		}
		sample.GPU, sample.HasGPU = clamp(s.gpu), s.hasGPU
		if s.gpuBusy && now.Sub(s.gpuAt) > 3*gpuFresh {
			// A reading that hangs, such as a stuck tool, shows as none
			// rather than as an old value.
			sample.GPU, sample.HasGPU = 0, false
		}
	}
	s.last, s.lastAt = sample, now
	return sample, nil
}

// readGPU takes a graphics reading in the background and keeps it for the
// next reading.
func (s *Sampler) readGPU() {
	ctx, cancel := context.WithTimeout(context.Background(), gpuTimeout)
	gpu, ok := s.r.gpu(ctx)
	cancel()
	s.mu.Lock()
	s.gpu, s.hasGPU, s.gpuBusy = gpu, ok, false
	s.mu.Unlock()
}

// share is the busy part of the ticks between two readings, in percent.
func share(before, after cpuTimes) float64 {
	if after.total <= before.total || after.busy < before.busy {
		return 0
	}
	return clamp(100 * float64(after.busy-before.busy) / float64(after.total-before.total))
}

func clamp(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	return min(v, 100)
}
