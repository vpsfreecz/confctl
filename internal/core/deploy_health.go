package core

import (
	"fmt"
	"math/big"

	"github.com/vpsfreecz/confctl/internal/cli"
)

// Called only at a reached nonempty health round. Preserve the text separately
// for Nix; the existing health declaration/CLI parser owns Integer admission.
// Parsing this synthetic invocation performs no selection, confirmation or run.
func deployHealthWorkers(inv cli.Invocation) (*big.Int, error) {
	value := inv.Options["max-jobs"].Value
	if value.Kind == cli.Null {
		return big.NewInt(5), nil
	}
	if value.Kind == cli.Number {
		return new(big.Int).Set(&value.Integer), nil
	}
	parsed, err := cli.BuiltinRegistry().Parse([]string{"health-check", "--max-jobs", value.String})
	if err != nil {
		return nil, fmt.Errorf("invalid health max-jobs: %w", err)
	}
	n := parsed.Options["max-jobs"].Value
	return new(big.Int).Set(&n.Integer), nil
}
