package main

import (
	ext "github.com/vpsfreecz/confctl/extension"
	"github.com/vpsfreecz/confctl/site"
)

func main() {
	ext.Serve(site.Handlers(), []string{"machines.select", "exec.run", "ui.write", "ui.machines", "ui.table", "ui.confirm", "format.nix"})
}
