package core

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"

	"github.com/vpsfreecz/confctl/internal/cli"
	"github.com/vpsfreecz/confctl/internal/gendata"
)

// GenData runs the three vpsAdmin leaves on the invocation's original streams.
// There is no confirmation step. One retained reader serves username, password
// and every later challenge; prompting and warning output never enter the log.
// stdout and stderr are borrowed, unbuffered original files in Main.
func (e *Engine) GenData(inv cli.Invocation, stdin *os.File, stdout, stderr io.Writer) error {
	return e.genDataCommand(inv, stdin, stdout, stderr, gendata.NewVPSAdminWithWarning)
}

func (e *Engine) LogGenDataCLI(inv cli.Invocation) error { return e.LogTestConnectionCLI(inv) }

// The factory seam is only for disposable HTTP/TLS tests. Production uses the
// ordinary fixed-origin provider, which owns auth, HTTP trust and request limits.
func (e *Engine) genDataCommand(inv cli.Invocation, stdin *os.File, stdout, stderr io.Writer,
	newClient func(gendata.Prompt, func(string) error) (*gendata.VPSAdmin, error),
) error {
	path := inv.Command.Path
	if len(path) != 3 || path[0] != "gen-data" || path[1] != "vpsadmin" {
		return errors.New("invalid gen-data command")
	}
	var leaf genDataLeaf
	switch path[2] {
	case "containers":
		leaf = genDataContainers
	case "network":
		leaf = genDataNetwork
	case "all":
		leaf = genDataAll
	default:
		return errors.New("invalid gen-data command")
	}
	input := bufio.NewReader(stdin)
	prompt := newLinuxGenDataPrompt(stdin, input, stdout, func() error { return nil })
	warn := gendata.WarningTo(stderr)
	return runGenData(e.Context, e.Root, leaf, genDataBindings{
		Prompt: prompt.Ask,
		NewClient: func(prompt gendata.Prompt, _ func(string)) (*gendata.VPSAdmin, error) {
			// Capture the synchronous sink deliberately. The legacy void warning
			// callback cannot propagate a failed write and is never forwarded.
			return newClient(prompt, warn)
		},
		Machines: func(context.Context) ([]Machine, error) { return e.Machines(false) },
	})
}
