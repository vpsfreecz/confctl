//go:build linux

package core

import (
	"bufio"
	"context"
	"os"
)

// Inert binding only: the existing plain-line owner acquires, wakes, joins
// and closes its own cancellation pipe on each call. stdin stays borrowed.
func linuxCommandLine(stdin *os.File, input *bufio.Reader) commandLine {
	native := nativeBasicLinuxOps()
	ops := inputWaitOps{Pipe: native.Pipe, Poll: native.Poll, Byte: native.Byte, Wake: native.Wake, Close: native.Close}
	return func(ctx context.Context) (string, error) {
		return linuxGenDataLine(ctx, stdin, input, ops)
	}
}
