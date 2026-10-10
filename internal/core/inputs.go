package core

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/vpsfreecz/confctl/internal/inputs"
)

type inputsArgumentError struct{ message string }

func (e *inputsArgumentError) Error() string { return e.message }

// Keep ordered channel JSON at this adapter boundary. Existing evaluator
// settings, impure policy and Nix fallback remain the authority for argv.
func (n *evaluator) inputJSON(installable string) ([]byte, error) {
	if err := n.argsSettings(); err != nil {
		return nil, err
	}
	return n.e.nix("eval", []string{installable}, nested(n.settings, "nix", "impureEval") == true, n.e.ShowTrace, n.max)
}

func (e *Engine) inputResolver() (*inputs.MachineResolver, error) {
	n, err := e.evaluator()
	if err != nil {
		return nil, err
	}
	return &inputs.MachineResolver{Eval: n.inputJSON}, nil
}

func (e *Engine) ListInputs(channel bool, args []string) (string, error) {
	// The source checks the flake before channel arity and before lock reads.
	if err := e.required(); err != nil {
		return "", err
	}
	if channel && len(args) > 1 {
		return "", &inputsArgumentError{"usage: confctl inputs channel ls [channel-pattern]"}
	}
	lock, err := inputs.LoadLock(filepath.Join(e.Root, "flake.lock"))
	if err != nil {
		return "", inputLockError(err, filepath.Join(e.Root, "flake.lock"))
	}
	var pattern *string
	if len(args) > 0 {
		pattern = &args[0]
	}
	rows := []map[string]any{}
	if !channel {
		names, err := lock.RootInputs()
		if err != nil {
			return "", err
		}
		for _, name := range names {
			info, err := lock.InputInfo(name)
			if err != nil {
				return "", err
			}
			if pattern != nil && !Match(*pattern, name) {
				continue
			}
			rows = append(rows, map[string]any{"input": name, "type": info.Type, "ref": info.Ref, "rev": info.ShortRev, "url": info.URL})
		}
		return Table(rows, []string{"input", "type", "ref", "rev", "url"}, true), nil
	}
	n, err := e.evaluator()
	if err != nil {
		return "", err
	}
	b, err := n.inputJSON(".#confctl.channels")
	if err != nil {
		return "", err
	}
	channels, err := inputs.DecodeChannels(b)
	if err != nil {
		return "", err
	}
	selected := inputs.SelectChannels(channels, pattern, Match)
	for _, target := range inputs.ChannelTargets(selected, nil) {
		info, err := lock.InputInfo(target.Input)
		if err != nil {
			return "", err
		}
		rows = append(rows, map[string]any{"channel": target.Channel, "role": target.Role, "input": target.Input, "rev": info.ShortRev, "url": info.URL})
	}
	return Table(rows, []string{"channel", "role", "input", "rev", "url"}, true), nil
}

func inputLockError(err error, path string) error {
	var fileError *os.PathError
	if errors.As(err, &fileError) && fileError.Op == "open" {
		return configurationError(err, "rb_sysopen", path)
	}
	return err
}
