package sysload

import (
	"context"
	"testing"
	"time"
)

// The Mach host statistics answer through the trampolines: processor ticks
// that move forward and memory within the machine's size.
func TestThisMac(t *testing.T) {
	first, err := machCPU()
	if err != nil || first.total == 0 || first.busy > first.total {
		t.Fatalf("processor ticks: %+v %v", first, err)
	}
	time.Sleep(50 * time.Millisecond)
	if second, err := machCPU(); err != nil || second.total < first.total {
		t.Fatalf("ticks went back: %+v then %+v (%v)", first, second, err)
	}
	used, total, err := machMemory()
	if err != nil || total == 0 || used == 0 || used > total {
		t.Fatalf("memory: %d of %d (%v)", used, total, err)
	}
	got, err := New().Read(context.Background())
	if err != nil || got.Cores < 1 || got.CPU < 0 || got.CPU > 100 || got.Memory() <= 0 {
		t.Fatalf("reading: %+v %v", got, err)
	}
	t.Logf("this Mac: cpu %.1f%%, memory %.1f%% of %d GB, gpu %.0f%% (%v)", got.CPU, got.Memory(), got.MemTotal>>30, got.GPU, got.HasGPU)
}

func TestIoreg(t *testing.T) {
	out := []byte(`+-o AGXAcceleratorG13X  <class AGXAcceleratorG13X>
    | "PerformanceStatistics" = {"In use system memory"=123,"Device Utilization %"=17,"Renderer Utilization %"=15}
+-o AMDRadeonX6000  <class AMD>
    | "PerformanceStatistics" = {"Device Utilization %" = 42}`)
	if v, ok := parseIoreg(out); !ok || v != 42 {
		t.Fatalf("ioreg: %v %v", v, ok)
	}
	if _, ok := parseIoreg([]byte("nothing here")); ok {
		t.Fatal("no accelerator parsed")
	}
}
