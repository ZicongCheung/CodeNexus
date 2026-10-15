//go:build windows

package traecn

import "syscall"

// createNoWindow keeps a console child from flashing a window when the gateway
// itself runs without a console (a windowed build).
const createNoWindow = 0x08000000

func childSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
