package hookcheck

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
)

func readPrepared(ctx context.Context, input PreparedInput) (preparedContext, error) {
	var p preparedContext
	if err := ctx.Err(); err != nil {
		return p, err
	}
	b, _, err := readOwnedRegular(input.ContextPath, true)
	if err != nil {
		return p, err
	}
	if sha256.Sum256(b) != input.SHA256 {
		return p, refuse("stale-identity", input.ContextPath)
	}
	if err = decodeCanonical(b, &p); err != nil {
		return p, refuse("invalid-receipt", input.ContextPath)
	}
	// toolsFromRecord indexes fixed roles. Validate their complete schema first.
	if err = validateRecord(p.Trust); err != nil {
		return p, refuse("invalid-receipt", input.ContextPath)
	}
	return p, ctx.Err()
}

// LoadPreparedTools borrows the authorized parent's private invocation context.
// Its hash is an integrity binding, not a new trust grant. RunHook still compares
// the independently fixed tools and operator-approved receipt before launch.
func LoadPreparedTools(ctx context.Context, input PreparedInput) (ToolSet, error) {
	return loadPreparedTools(ctx, input, func() (string, error) {
		path, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(path)
	}, identifyLocalTool)
}

func loadPreparedTools(ctx context.Context, input PreparedInput,
	self func() (string, error), identify func(context.Context, ToolIdentity) (ToolIdentity, error),
) (ToolSet, error) {
	p, err := readPrepared(ctx, input)
	if err != nil {
		return ToolSet{}, err
	}
	tools := toolsFromRecord(p.Trust)
	if err = validatePrepared(p, input, tools); err != nil {
		return ToolSet{}, err
	}
	path, err := self()
	if err != nil {
		return ToolSet{}, err
	}
	if path != tools.Checker.Resolved {
		return ToolSet{}, refuse("stale-identity", path)
	}
	actual, err := identify(ctx, tools.Checker)
	if err != nil {
		return ToolSet{}, err
	}
	if actual != tools.Checker {
		return ToolSet{}, refuse("stale-identity", tools.Checker.Program)
	}
	if err = ctx.Err(); err != nil {
		return ToolSet{}, err
	}
	return copyTools(tools), nil
}
