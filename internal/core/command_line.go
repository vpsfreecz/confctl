package core

import (
	"bufio"
	"context"
	"io"
	"os"
)

type commandLine func(context.Context) (string, error)

// Capture the borrowed file before buffering. The same reader must survive
// later confirmations, deployment choices and Basic pager input.
func commandInput(in io.Reader) (*bufio.Reader, commandLine) {
	file, native := in.(*os.File)
	input, ok := in.(*bufio.Reader)
	if !ok {
		input = bufio.NewReader(in)
	}
	if native {
		return input, linuxCommandLine(file, input)
	}
	// Generic readers keep the synchronous fixture/API contract. An arbitrary
	// blocking io.Reader cannot be interrupted without owning its lifetime.
	return input, func(ctx context.Context) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return input.ReadString('\n')
	}
}
