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
			code int
		}{{"empty", nil, prior}, {"SIGINT", os.Interrupt, 130}, {"SIGTERM", syscall.SIGTERM, 143}} {
			t.Run(fmt.Sprintf("%d/%s", prior, tc.name), func(t *testing.T) {
				received := make(chan os.Signal, 1)
				if tc.sig != nil {
					received <- tc.sig
				}
				if got, signaled := signalExitStatus(prior, received); got != tc.code || signaled != (tc.sig != nil) || len(received) != 0 {
					t.Fatal(got, signaled, len(received), "want", tc.code, tc.sig != nil)
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
