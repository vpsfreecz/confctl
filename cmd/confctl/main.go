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
	code := core.Main(ctx, os.Args[1:])
	select {
	case <-received:
		code = 1
	default:
	}
	os.Exit(code)
}
