package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestInvocationTimingSignalExitStatus(t *testing.T) {
	for _, prior := range []int{0, 7, 130, 143} {
		for _, tc := range []struct {
			name string
			sig  os.Signal
		}{{"empty", nil}, {"SIGINT", os.Interrupt}, {"SIGTERM", syscall.SIGTERM}} {
			t.Run(fmt.Sprintf("%d/%s", prior, tc.name), func(t *testing.T) {
				received := make(chan os.Signal, 1)
				want, wantSignal := prior, tc.sig != nil
				if wantSignal {
					received <- tc.sig
					want = 1
				}
				if got, signaled := signalExitStatus(prior, received); got != want || signaled != wantSignal || len(received) != 0 {
					t.Fatal(got, signaled, len(received), "want", want, wantSignal)
				}
				if got, signaled := signalExitStatus(23, received); got != 23 || signaled {
					t.Fatal("signal was reused", got, signaled)
				}
			})
		}
		if got, signaled := signalExitStatus(prior, nil); got != prior || signaled {
			t.Fatal("nil channel changed status", got, signaled)
		}
	}
}
