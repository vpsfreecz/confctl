package main

import (
	"fmt"
	"io"
	"os"

	"github.com/vpsfreecz/confctl/internal/netboot"
)

// The package binds an immutable store config. No CLI/environment override is
// exposed, and the original entrypoint's ignored argv remain ignored.
var configFile string

func execute(path string, _ []string, d netboot.Dependencies, stderr io.Writer) int {
	b, err := d.FS.Read(path)
	if err == nil {
		var c netboot.Config
		c, err = netboot.ParseConfig(b)
		if err == nil {
			err = netboot.Run(c, d)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "build-netboot-server: %v\n", err)
		return 1
	}
	return 0
}
func main() { os.Exit(execute(configFile, os.Args[1:], netboot.OSDependencies(), os.Stderr)) }
