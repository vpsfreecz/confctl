// Fixture-only hook entrypoint. Native deploy/rediscover stay unavailable.
package main

import (
	"context"
	"fmt"
	"github.com/vpsfreecz/confctl/internal/core"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	r, err := core.LoadRegistry()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	e, err := core.New(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err = e.OpenLog("compat-hook"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	names := []string{}
	if len(os.Args) > 2 && os.Args[2] != "" {
		names = strings.Split(os.Args[2], ",")
	}
	code, err := e.Hooks(r, os.Args[1], names)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	e.CloseLog(code == 0)
	os.Exit(code)
}
