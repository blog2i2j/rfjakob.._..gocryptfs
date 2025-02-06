//go:build !((linux && 386) || (linux && arm))

package syscallcompat

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// See thread_credentials.go for docs

func Setreuid(ruid int, euid int) (err error) {
	_, _, e1 := unix.RawSyscall(unix.SYS_SETREUID, uintptr(ruid), uintptr(euid), 0)
	if e1 != 0 {
		err = e1
	}
	return
}

func Setregid(rgid int, egid int) (err error) {
	_, _, e1 := unix.RawSyscall(unix.SYS_SETREGID, uintptr(rgid), uintptr(egid), 0)
	if e1 != 0 {
		err = e1
	}
	return
}

func setgroups(n int, list *uint32) (err error) {
	_, _, e1 := unix.RawSyscall(unix.SYS_SETGROUPS, uintptr(n), uintptr(unsafe.Pointer(list)), 0)
	if e1 != 0 {
		err = e1
	}
	return
}
