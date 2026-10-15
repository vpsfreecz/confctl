package core

import "os"

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
