//go:build !windows

package traecn

import "syscall"

// childSysProcAttr returns no special attributes off Windows; the child shares
// the parent's session, which is what a CLI expects.
func childSysProcAttr() *syscall.SysProcAttr { return nil }
