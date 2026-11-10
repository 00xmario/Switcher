package sysload

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func fakeSampler(ticks *[]cpuTimes, gpuCalls *int, clock *fakeClock) *Sampler {
	return &Sampler{now: clock.now, r: reader{
		cpu: func() (cpuTimes, error) {
			if len(*ticks) == 0 {
				return cpuTimes{}, errors.New("no more ticks")
			}
			t := (*ticks)[0]
			*ticks = (*ticks)[1:]
			clock.t = clock.t.Add(window)
			return t, nil
		},
		memory: func() (uint64, uint64, error) { return 6 << 30, 8 << 30, nil },
		gpu:    func(context.Context) (float64, bool) { *gpuCalls++; return 37.4, true },
		cores:  func() int { return 8 },
	}}
}

// The first reading measures the processor over a short wait; one asked
// again within a moment is the same reading, and a later one measures from
// the reading before.
func TestReadingsMeasureTheProcessorBetweenTwoTicks(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	ticks := []cpuTimes{{busy: 100, total: 1000}, {busy: 150, total: 1100}, {busy: 250, total: 1200}}
	gpuCalls := 0
	s := fakeSampler(&ticks, &gpuCalls, clock)
	got, err := s.Read(context.Background())
	if err != nil || got.CPU != 50 || got.Cores != 8 || got.Memory() != 75 || !got.HasGPU || got.GPU != 37.4 {
		t.Fatalf("first reading: %+v %v", got, err)
	}
	again, _ := s.Read(context.Background())
	if again != got || len(ticks) != 1 {
		t.Fatalf("a reading within a moment was taken again: %+v, ticks left %d", again, len(ticks))
	}
	clock.t = clock.t.Add(2 * time.Second)
	got, err = s.Read(context.Background())
	if err != nil || got.CPU != 100 || gpuCalls != 2 {
		t.Fatalf("second reading: %+v %v gpu calls %d", got, err, gpuCalls)
	}
}

// Ticks from long ago would show an average, not the load now: the sampler
// measures afresh.
func TestOldTicksAreMeasuredAfresh(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	ticks := []cpuTimes{{busy: 0, total: 100}, {busy: 100, total: 200}, {busy: 100, total: 10200}, {busy: 110, total: 10300}}
	gpuCalls := 0
	s := fakeSampler(&ticks, &gpuCalls, clock)
	if got, _ := s.Read(context.Background()); got.CPU != 100 {
		t.Fatalf("first: %v", got.CPU)
	}
	clock.t = clock.t.Add(time.Minute)
	if got, _ := s.Read(context.Background()); got.CPU != 10 {
		t.Fatalf("after a quiet minute: %v", got.CPU)
	}
}

func TestAReadingWaitsNoLongerThanItsCaller(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1000, 0)}
	ticks := []cpuTimes{{busy: 0, total: 100}, {busy: 1, total: 200}}
	gpuCalls := 0
	s := fakeSampler(&ticks, &gpuCalls, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}

func TestShareStaysInRange(t *testing.T) {
	for _, c := range []struct {
		before, after cpuTimes
		want          float64
	}{
		{cpuTimes{10, 100}, cpuTimes{10, 100}, 0},    // no time passed
		{cpuTimes{10, 100}, cpuTimes{5, 200}, 0},     // counters went back
		{cpuTimes{10, 100}, cpuTimes{500, 200}, 100}, // more busy than total
		{cpuTimes{10, 100}, cpuTimes{35, 200}, 25},
	} {
		if got := share(c.before, c.after); got != c.want {
			t.Errorf("share(%v, %v) = %v, want %v", c.before, c.after, got, c.want)
		}
	}
}

func TestAnUnsupportedPlatformSaysSo(t *testing.T) {
	var s *Sampler
	if _, err := s.Read(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("nil sampler: %v", err)
	}
	if _, err := (&Sampler{}).Read(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("empty sampler: %v", err)
	}
}

func TestViewsRoundAndSanitize(t *testing.T) {
	v := ViewOf(Sample{CPU: 12.5, Cores: 4, MemUsed: 3, MemTotal: 4, GPU: 99.6, HasGPU: true}, "k12", "linux")
	if !v.Available || v.CPU != 13 || v.Memory != 75 || v.GPU == nil || *v.GPU != 100 || v.Machine != "k12" {
		t.Fatalf("view: %+v", v)
	}
	if v = ViewOf(Sample{CPU: 1}, "", "darwin"); v.GPU != nil {
		t.Fatalf("a missing graphics reading shows: %+v", v)
	}
	gpu := -5
	v = View{Machine: "x", Platform: "plan9", Reason: "<b>", Available: true, CPU: 101, Memory: -1, Cores: 99999, MemUsed: 1, MemTotal: 1 << 50, GPU: &gpu}
	v.Sanitize()
	if v.Machine != "" || v.Platform != "" || v.Reason != "" || v.CPU != 100 || v.Memory != 0 || v.Cores != 0 || v.MemTotal != 0 || *v.GPU != 0 {
		t.Fatalf("sanitized: %+v", v)
	}
}
