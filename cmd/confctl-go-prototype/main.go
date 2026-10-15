package main

import (
	"context"
	"github.com/vpsfreecz/confctl/internal/core"
	"os"
	"os/signal"
	"syscall"
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
	case sig := <-received:
		if sig == syscall.SIGTERM {
			code = 143
		} else {
			code = 130
		}
	default:
	}
	os.Exit(code)
}
