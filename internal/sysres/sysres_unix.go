//go:build linux || darwin || freebsd

package sysres

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// residentMB reads current RSS on Linux; returns 0 elsewhere (caller falls back).
func residentMB() float64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseFloat(f[1], 64)
	if err != nil {
		return 0
	}
	return pages * float64(os.Getpagesize()) / (1 << 20)
}
