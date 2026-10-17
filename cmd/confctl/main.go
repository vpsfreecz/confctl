package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/vpsfreecz/confctl/internal/core"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	received := make(chan os.Signal, 1)
	go func() {
		select {
		case sig := <-signals:
			received <- sig
			cancel()
		case <-ctx.Done():
		}
	}()
	code := core.MainWithExitPolicy(ctx, os.Args[1:], func(code int) (int, bool) {
		return signalExitStatus(code, received)
	})
	os.Exit(code)
}

func signalExitStatus(code int, received <-chan os.Signal) (int, bool) {
	select {
	case <-received:
		return 1, true
	default:
		return code, false
	}
}
