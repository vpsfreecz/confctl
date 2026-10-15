package health

import (
	"math/big"
	"reflect"
	"testing"
)

func text(value string) *string { return &value }

func TestCommandPredicatesSourcePriorityAndEmptyFragments(t *testing.T) {
	for _, test := range []struct {
		name           string
		out, err       Output
		stdout, stderr string
		want           *Failure
	}{
		{"stdout match before all others, no trim", Output{Match: text("out"), Include: []string{"missing"}}, Output{Match: text("wrong")}, "out\n", "bad", &Failure{Kind: "match", Stream: "standard output", Fragment: "out"}},
		{"empty exact match is enabled", Output{Match: text("")}, Output{}, "x", "", &Failure{Kind: "match", Stream: "standard output", Fragment: ""}},
		{"first missing include", Output{Include: []string{"", "first", "second"}, Exclude: []string{"out"}}, Output{Match: text("wrong")}, "out", "", &Failure{Kind: "include", Stream: "standard output", Fragment: "first"}},
		{"first present exclude including empty", Output{Exclude: []string{"absent", "", "out"}}, Output{Match: text("wrong")}, "out", "", &Failure{Kind: "exclude", Stream: "standard output", Fragment: ""}},
		{"stderr match", Output{}, Output{Match: text("err"), Include: []string{"missing"}}, "out", "err\n", &Failure{Kind: "match", Stream: "standard error", Fragment: "err"}},
		{"stderr include", Output{}, Output{Include: []string{"first", "second"}, Exclude: []string{"err"}}, "out", "err", &Failure{Kind: "include", Stream: "standard error", Fragment: "first"}},
		{"stderr exclude", Output{}, Output{Exclude: []string{"err", ""}}, "out", "err", &Failure{Kind: "exclude", Stream: "standard error", Fragment: "err"}},
		{"all conditions pass with full bytes", Output{Match: text("out\x00\n"), Include: []string{"", "\x00"}, Exclude: []string{"missing"}}, Output{Match: text("")}, "out\x00\n", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := &Command{ExpectedStatus: big.NewInt(0), Stdout: test.out, Stderr: test.err}
			got := commandFailures(command, CommandResult{Status: 9, Stdout: []byte(test.stdout), Stderr: []byte(test.stderr)})
			want := []Failure{{Kind: "status", Status: 9}}
			if test.want != nil {
				want = append(want, *test.want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("failures=%#v want %#v", got, want)
			}
		})
	}
	if got := commandFailures(&Command{ExpectedStatus: big.NewInt(255)}, CommandResult{Status: 255}); len(got) != 0 {
		t.Fatalf("ordinary child/SSH 255 must be configurable status data: %#v", got)
	}
}
