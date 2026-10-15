// Package health runs private, ordered command and systemd health checks.
package health

import (
	"errors"
	"fmt"
	"math/big"
)

var ErrNoChecks = errors.New("No runnable machines to check or no health checks configured")

type SchemaError struct{ Path, Problem string }

func (e *SchemaError) Error() string { return e.Path + ": " + e.Problem }

type Timing struct {
	Timeout, Cooldown *big.Int // Nonnegative seconds, without duration truncation.
}

type Check struct {
	Machine, Description string
	Argv                 []string
	Remote               bool
	Timing
	Command    *Command
	Properties []Property
	Pattern    *string
}

type Output struct {
	Match            *string
	Include, Exclude []string
}

type Command struct {
	Description    string // Original description, including the empty fallback case.
	ExpectedStatus *big.Int
	Stdout, Stderr Output
}

type Property struct {
	Name, Value string
	Timing
}

type CommandResult struct {
	Status         int
	Stdout, Stderr []byte
}

// Failure retains the first offending fragment even when that fragment is empty.
// Core owns source-compatible diagnostics; the runner does not need a shell or
// a Ruby string renderer to evaluate predicates.
type Failure struct {
	Kind, Stream, Fragment string
	Property, Actual, Want string
	Status                 int
}

func (c Check) validate() error {
	if len(c.Argv) == 0 {
		return &SchemaError{c.Machine, "health command must not be empty"}
	}
	if c.Timeout == nil || c.Cooldown == nil || c.Timeout.Sign() < 0 || c.Cooldown.Sign() < 0 {
		return &SchemaError{c.Machine, "health timing must contain nonnegative seconds"}
	}
	if c.Command != nil {
		if len(c.Properties) != 0 || c.Command.ExpectedStatus == nil || c.Command.ExpectedStatus.Sign() < 0 {
			return &SchemaError{c.Machine, "invalid command health specification"}
		}
	} else if len(c.Properties) == 0 {
		return &SchemaError{c.Machine, "systemd property group must not be empty"}
	}
	return nil
}

// WorkerCount compares the full requested integer before any native conversion.
func WorkerCount(request *big.Int, checks int) (int, error) {
	if request == nil || request.Sign() <= 0 {
		return 0, fmt.Errorf("max-jobs must be greater than zero")
	}
	if checks < 0 {
		return 0, fmt.Errorf("negative health check count")
	}
	if request.Cmp(big.NewInt(int64(checks))) >= 0 {
		return checks, nil
	}
	return int(request.Int64()), nil
}
