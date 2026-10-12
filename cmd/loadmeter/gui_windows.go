//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// showError displays an error in a message box; used by the windowless
// (GUI subsystem) build, which has no console to print to.
func showError(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(msg)
	const mbOK, mbIconError = 0x0, 0x10
	proc.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbOK|mbIconError)
}
