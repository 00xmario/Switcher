package sysload

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS keeps processor ticks and memory pages in the Mach host statistics,
// which only libSystem's host_statistics calls hand out. They are called the
// way golang.org/x/sys calls libSystem without cgo: through an assembly
// trampoline (sysload_darwin.s) and the runtime's libc call.

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var libc_mach_host_self_trampoline_addr uintptr

//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"

var libc_host_statistics_trampoline_addr uintptr

//go:cgo_import_dynamic libc_host_statistics host_statistics "/usr/lib/libSystem.B.dylib"

var libc_host_statistics64_trampoline_addr uintptr

//go:cgo_import_dynamic libc_host_statistics64 host_statistics64 "/usr/lib/libSystem.B.dylib"

const (
	hostCPULoadInfo = 3 // HOST_CPU_LOAD_INFO
	hostVMInfo64    = 4 // HOST_VM_INFO64
)

// vmStatistics64 mirrors struct vm_statistics64 in <mach/vm_statistics.h>;
// Go lays it out the same way (natural_t is uint32, 8-byte aligned uint64s).
type vmStatistics64 struct {
	freeCount                          uint32
	activeCount                        uint32
	inactiveCount                      uint32
	wireCount                          uint32
	zeroFillCount                      uint64
	reactivations                      uint64
	pageins                            uint64
	pageouts                           uint64
	faults                             uint64
	cowFaults                          uint64
	lookups                            uint64
	hits                               uint64
	purges                             uint64
	purgeableCount                     uint32
	speculativeCount                   uint32
	decompressions                     uint64
	compressions                       uint64
	swapins                            uint64
	swapouts                           uint64
	compressorPageCount                uint32
	throttledCount                     uint32
	externalPageCount                  uint32
	internalPageCount                  uint32
	totalUncompressedPagesInCompressor uint64
}

// hostPort is this task's send right to the host, taken once: every
// mach_host_self call adds a reference.
var hostPort = sync.OnceValue(func() uintptr {
	port, _, _ := syscall_syscall6(libc_mach_host_self_trampoline_addr, 0, 0, 0, 0, 0, 0)
	return port
})

func platform() reader {
	return reader{cpu: machCPU, memory: machMemory, gpu: ioregGPU, cores: runtime.NumCPU}
}

// machCPU reads the processor ticks of all cores: user, system, idle, nice.
func machCPU() (cpuTimes, error) {
	var ticks [4]uint32
	count := uint32(len(ticks))
	kr, _, _ := syscall_syscall6(libc_host_statistics_trampoline_addr, hostPort(), hostCPULoadInfo,
		uintptr(unsafe.Pointer(&ticks[0])), uintptr(unsafe.Pointer(&count)), 0, 0)
	if kr != 0 {
		return cpuTimes{}, syscall.Errno(kr)
	}
	busy := uint64(ticks[0]) + uint64(ticks[1]) + uint64(ticks[3])
	return cpuTimes{busy: busy, total: busy + uint64(ticks[2])}, nil
}

// machMemory reads memory in use as Activity Monitor counts it: app memory
// (anonymous pages that cannot be purged), wired memory, and the compressor.
// File cache and purgeable pages are free to take back.
func machMemory() (used, total uint64, err error) {
	total, err = unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, 0, err
	}
	var vm vmStatistics64
	count := uint32(unsafe.Sizeof(vm) / 4)
	kr, _, _ := syscall_syscall6(libc_host_statistics64_trampoline_addr, hostPort(), hostVMInfo64,
		uintptr(unsafe.Pointer(&vm)), uintptr(unsafe.Pointer(&count)), 0, 0)
	if kr != 0 {
		return 0, 0, syscall.Errno(kr)
	}
	page := uint64(os.Getpagesize())
	if size, err := unix.SysctlUint32("hw.pagesize"); err == nil && size > 0 {
		page = uint64(size)
	}
	app := uint64(vm.internalPageCount) - min(uint64(vm.purgeableCount), uint64(vm.internalPageCount))
	used = (app + uint64(vm.wireCount) + uint64(vm.compressorPageCount)) * page
	return min(used, total), total, nil
}

// utilization is the busy share every macOS graphics driver reports in its
// accelerator's PerformanceStatistics.
var utilization = regexp.MustCompile(`"Device Utilization %"\s*=\s*([0-9]+)`)

// ioregGPU asks the I/O registry for the graphics accelerators' statistics
// and answers the busiest.
func ioregGPU(ctx context.Context) (float64, bool) {
	cmd := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	cmd.WaitDelay = 500 * time.Millisecond // a tool that ignores the kill does not hold the reading
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	return parseIoreg(out)
}

func parseIoreg(out []byte) (float64, bool) {
	best, found := 0.0, false
	for _, m := range utilization.FindAllSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(string(m[1]), 64); err == nil {
			best, found = max(best, clamp(v)), true
		}
	}
	return best, found
}
