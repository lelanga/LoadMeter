//go:build linux || darwin || freebsd

package main

import "syscall"

func fdLimit() uint64 {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0
	}
	return uint64(r.Cur)
}
