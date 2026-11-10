// Trampolines into libSystem for sysload_darwin.go, as golang.org/x/sys
// writes them. The same source assembles for arm64 and amd64.

//go:build darwin

#include "textflag.h"

TEXT libc_mach_host_self_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_mach_host_self(SB)
GLOBL	·libc_mach_host_self_trampoline_addr(SB), RODATA, $8
DATA	·libc_mach_host_self_trampoline_addr(SB)/8, $libc_mach_host_self_trampoline<>(SB)

TEXT libc_host_statistics_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics(SB)
GLOBL	·libc_host_statistics_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics_trampoline_addr(SB)/8, $libc_host_statistics_trampoline<>(SB)

TEXT libc_host_statistics64_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_host_statistics64(SB)
GLOBL	·libc_host_statistics64_trampoline_addr(SB), RODATA, $8
DATA	·libc_host_statistics64_trampoline_addr(SB)/8, $libc_host_statistics64_trampoline<>(SB)
