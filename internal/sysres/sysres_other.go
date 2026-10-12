//go:build !(linux || darwin || freebsd)

package sysres

import "time"

// CPU sampling is not implemented on this platform yet (reported as 0).
func processCPU() time.Duration { return 0 }
func residentMB() float64       { return 0 }
