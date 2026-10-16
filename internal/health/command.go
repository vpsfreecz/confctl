package health

import (
	"math/big"
	"strings"
)

func outputFailure(output Output, text, stream string) *Failure {
	if output.Match != nil && text != *output.Match {
		return &Failure{Kind: "match", Stream: stream, Fragment: *output.Match}
	}
	for _, fragment := range output.Include {
		if !strings.Contains(text, fragment) {
			return &Failure{Kind: "include", Stream: stream, Fragment: fragment}
		}
	}
	for _, fragment := range output.Exclude {
		if strings.Contains(text, fragment) {
			return &Failure{Kind: "exclude", Stream: stream, Fragment: fragment}
		}
	}
	return nil
}

func commandFailures(command *Command, result CommandResult) []Failure {
	var failures []Failure
	if command.ExpectedStatus.Cmp(big.NewInt(int64(result.Status))) != 0 {
		failures = append(failures, Failure{Kind: "status", Status: result.Status})
	}
	// One output failure at most, after the independently recorded status failure.
	if failure := outputFailure(command.Stdout, string(result.Stdout), "standard output"); failure != nil {
		return append(failures, *failure)
	}
	if failure := outputFailure(command.Stderr, string(result.Stderr), "standard error"); failure != nil {
		return append(failures, *failure)
	}
	return failures
}
