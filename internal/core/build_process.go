package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func (e *Engine) buildProcess(argv []string, input []byte, chunk func([]byte, []byte) error) (Result, error) {
	return e.processStreams(argv, input, nil, chunk)
}

// processStreams is the shared checked-log wrapper for build, GC and deploy.
// Context/root/log ownership come from this Engine. Task callers copy both Log
// and logOwner when binding another context; env is command-local (nil inherits).
func (e *Engine) processStreams(argv []string, input []byte, env []string, chunk func(stdout, stderr []byte) error) (Result, error) {
	id, start, err := e.processLogStart(argv)
	if err != nil {
		return Result{}, err
	}
	r, err := processWithStreamsEnv(e.Context, e.Root, argv, input, env, func(stdout, stderr []byte) error {
		b := stdout
		if stderr != nil {
			b = stderr
		}
		if err := e.logText("[" + id + "] \t" + strings.TrimSuffix(string(b), "\n") + "\n"); err != nil {
			return err
		}
		if chunk != nil {
			return chunk(stdout, stderr)
		}
		return nil
	})
	if _, err = buildStatus(e.Context, r, err); err != nil {
		return r, err
	}
	return r, e.processLogFinish(id, start, r)
}

func buildStatus(ctx context.Context, r Result, runErr error) (int, error) {
	if primary := r.capturePrimary(); primary != nil {
		return 0, primary
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if runErr != nil {
		return 0, runErr
	}
	if r.waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(r.waitErr, &exit) {
			return 0, r.waitErr
		}
	}
	if r.signal != 0 {
		return int(r.signal), nil
	}
	if r.ExitCode < 0 {
		return 0, fmt.Errorf("build command did not return an exit status")
	}
	return r.ExitCode, nil
}
