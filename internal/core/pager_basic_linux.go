package core

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// SyscallConn exposes the borrowed descriptor without File.Fd's possible
// blocking-mode change. No terminal or descriptor is opened by this helper.
func basicBorrowedFD(file *os.File) (uintptr, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var fd uintptr
	err = connection.Control(func(value uintptr) { fd = value })
	return fd, err
}

func basicNativeTermios(file *os.File, request uintptr, attributes *unix.Termios) error {
	fd, err := basicBorrowedFD(file)
	if err != nil {
		return err
	}
	switch request {
	case unix.TCGETS:
		var saved *unix.Termios
		saved, err = unix.IoctlGetTermios(int(fd), unix.TCGETS)
		if err == nil {
			*attributes = *saved
		}
	case unix.TCSETS:
		err = unix.IoctlSetTermios(int(fd), unix.TCSETS, attributes)
	default:
		err = unix.EINVAL
	}
	runtime.KeepAlive(file)
	return err
}
