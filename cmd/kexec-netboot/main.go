package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vpsfreecz/confctl/internal/kexecnetboot"
)

// Both values are immutable package bindings, never environment overrides.
var machineFqdn, kexecTools string

func execute(ctx context.Context, program string, args []string, ordered bool, c kexecnetboot.Config, d kexecnetboot.Dependencies) int {
	o, p := kexecnetboot.Parse(program, args, ordered)
	if p != nil {
		fmt.Fprint(d.Out, p.Stdout)
		fmt.Fprint(d.Err, p.Stderr)
		return p.Status
	}
	status, err := kexecnetboot.Run(ctx, c, o, d)
	if err != nil {
		message := err.Error()
		if !strings.HasPrefix(message, "ERROR: ") {
			message = "ERROR: " + message
		}
		fmt.Fprintln(d.Err, message)
	}
	return status
}
func actionContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

func main() {
	_, ordered := os.LookupEnv("POSIXLY_CORRECT")
	d := kexecnetboot.OSDependencies()
	d.BeginActions = actionContext
	status := execute(context.Background(), os.Args[0], os.Args[1:], ordered, kexecnetboot.Config{MachineFQDN: machineFqdn, Kexec: kexecTools}, d)
	os.Exit(status)
}
