//go:build !(linux || darwin || freebsd)

package main

func fdLimit() uint64 { return 0 }
