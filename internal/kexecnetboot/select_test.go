package kexecnetboot

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// These literal byte contracts come from kexec-netboot.rb's display formats
// (221-375) and summary (86-100), using unchanged nominated fixture fields.
// They are source-derived expectations, not an observed original runtime.
const sourceMachineTable = "       FQDN                            SPIN\n" +
	" [1]*  node.test                       nixos\n" +
	"  [2]  other.test                      vpsadminos\n" +
	"Select a machine by number: "
const sourceGenerationTable = "Available generations for node.test: (default is 6 - current)\n" +
	"       TIME                 REVISION    KERNEL\n" +
	" [7]*  2024-10-01 00:00:00  012345678   6.6.52\n" +
	" [6]*  2024-10-01 00:00:00  012345678   6.6.52\n" +
	"  [5]  2024-10-01 00:00:00  012345678   6.6.52\n" +
	"Enter generation number (or negative offset) [ENTER for default]: "
const sourceVariantTable = "Variants available for generation 7:\n" +
	"       LABEL\n" +
	"  [1]  Default runlevel\n" +
	"  [2]  Default runlevel without container imports\n" +
	"  [3]  Default runlevel without container autostart\n" +
	"  [4]  Rescue runlevel (network and sshd)\n" +
	"  [5]  Single-user runlevel (console only)\n" +
	"Choose variant by number [ENTER for 'default']: "
const sourceSummaryNone = "\n" +
	"Selected configuration:\n" +
	"  Machine:       node.test (spin=nixos)\n" +
	"  Generation:    7 (2024-10-01 00:00:00, rev=012345678, kernel=6.6.52)\n" +
	"  Variant:       none\n" +
	"  Kernel params: httproot=http://netboot.test/node.test/7/initrd init=/fixture/store/system-7/init console=ttyS0 \n" +
	"\n"
const sourceSummaryVariant = "\n" +
	"Selected configuration:\n" +
	"  Machine:       node.test (spin=vpsadminos)\n" +
	"  Generation:    7 (2024-10-01 00:00:00, rev=012345678, kernel=6.6.52)\n" +
	"  Variant:       rescue (Rescue runlevel (network and sshd))\n" +
	"  Kernel params: httproot=http://netboot.test/node.test/7/initrd init=/fixture/store/system-7/init console=ttyS0 runlevel=rescue \n" +
	"\n"
const sourceSummaryVariantEOF = "\n" +
	"Selected configuration:\n" +
	"  Machine:       node.test (spin=vpsadminos)\n" +
	"  Generation:    7 (2024-10-01 00:00:00, rev=012345678, kernel=6.6.52)\n" +
	"  Variant:       none\n" +
	"  Kernel params: httproot=http://netboot.test/node.test/7/initrd init=/fixture/store/system-7/init console=ttyS0 \n" +
	"\n"

func variantMachine(t *testing.T) record {
	t.Helper()
	b, err := os.ReadFile("testdata/index-e6870cb0b0bd.json")
	if err != nil {
		t.Fatal(err)
	}
	index, err := readIndex(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return record(index["machines"].([]any)[0].(map[string]any))
}

func TestSelectionTableBytes(t *testing.T) {
	t.Run("machine-invalid-then-default", func(t *testing.T) {
		m := baseMachine(t)
		index := record{"machines": []any{map[string]any(m), map[string]any{"fqdn": "other.test", "spin": "vpsadminos"}}}
		var out, stderr bytes.Buffer
		s := selector{in: bufio.NewReader(strings.NewReader("99\n0\n")), out: &out, err: &stderr, interactive: true, defaultFQDN: "node.test"}
		selected, err := s.machine(index, nil)
		want := sourceMachineTable + "Invalid selection. Please try again.\n" + sourceMachineTable
		if err != nil || selected["fqdn"] != "node.test" || out.String() != want || stderr.Len() != 0 {
			t.Fatalf("selection=%v err=%v stdout=%q stderr=%q", selected, err, out.String(), stderr.String())
		}
		out.Reset()
		s.in = bufio.NewReader(strings.NewReader(""))
		selected, err = s.machine(index, nil)
		if err != nil || selected != nil || out.String() != sourceMachineTable {
			t.Fatalf("EOF selection=%v err=%v stdout=%q", selected, err, out.String())
		}
	})
	t.Run("generation-truthy-marks-exact-true-default", func(t *testing.T) {
		m := baseMachine(t)
		gs, _ := records(m["generations"], "generations", true)
		gs[0]["current"] = json.Number("1")
		gs[1]["current"] = true
		gs[2]["current"] = nil
		var out, stderr bytes.Buffer
		s := selector{in: bufio.NewReader(strings.NewReader("oops\n\n")), out: &out, err: &stderr, interactive: true}
		selected, err := s.generation(m, nil)
		want := sourceGenerationTable + "Invalid generation 'oops'. Please try again.\n" + sourceGenerationTable
		if err != nil || selected["generation"] != json.Number("6") || out.String() != want || stderr.Len() != 0 {
			t.Fatalf("selection=%v err=%v stdout=%q stderr=%q", selected, err, out.String(), stderr.String())
		}
		out.Reset()
		s.in = bufio.NewReader(strings.NewReader(""))
		selected, err = s.generation(m, nil)
		if err != nil || selected != nil || out.String() != sourceGenerationTable {
			t.Fatalf("EOF selection=%v err=%v stdout=%q", selected, err, out.String())
		}
	})
	t.Run("variant-invalid-then-numeric-prefix", func(t *testing.T) {
		m := variantMachine(t)
		gs, _ := records(m["generations"], "generations", true)
		var out, stderr bytes.Buffer
		s := selector{in: bufio.NewReader(strings.NewReader("bogus\n2tail\n")), out: &out, err: &stderr, interactive: true}
		selected, err := s.variant(gs[0], nil)
		want := sourceVariantTable + "Invalid selection. Please try again.\n" + sourceVariantTable
		if err != nil || selected["name"] != "nopools" || out.String() != want || stderr.Len() != 0 {
			t.Fatalf("selection=%v err=%v stdout=%q stderr=%q", selected, err, out.String(), stderr.String())
		}
		out.Reset()
		s.in = bufio.NewReader(strings.NewReader(""))
		selected, err = s.variant(gs[0], nil)
		if err != nil || selected != nil || out.String() != sourceVariantTable {
			t.Fatalf("EOF selection=%v err=%v stdout=%q", selected, err, out.String())
		}
	})
}

func TestInteractiveSummaryBytesAndEOF(t *testing.T) {
	for _, tc := range []struct {
		name, input, want        string
		status                   int
		eof, variant, variantEOF bool
	}{
		{name: "none-blank-other-then-abort", input: "\n?\nn\n", want: sourceSummaryNone + strings.Repeat("Continue? [y/N]: ", 3), status: 1},
		{name: "none-eof", want: sourceSummaryNone + "Continue? [y/N]: ", status: 1, eof: true},
		{name: "variant-eof", want: sourceSummaryVariant + "Continue? [y/N]: ", status: 1, eof: true, variant: true},
		{name: "variant-blank-case-whitespace-then-load", input: "\n Y \n", want: sourceSummaryVariant + strings.Repeat("Continue? [y/N]: ", 2) + "\n", variant: true},
		{name: "variant-prompt-eof-still-final-confirmation", want: sourceVariantTable + sourceSummaryVariantEOF + "Continue? [y/N]: ", status: 1, eof: true, variant: true, variantEOF: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := nominations(t)[0]
			if tc.variant {
				n.Literal.Index.Fixture = "index-e6870cb0b0bd.json"
			}
			n.Stdin = &tc.input
			f := mockRun(t, n)
			o := Options{Interactive: true, Machine: strptr("node.test"), Generation: strptr("7")}
			if tc.variant && !tc.variantEOF {
				o.Variant = strptr("rescue")
			}
			status, err := Run(context.Background(), Config{"node.test", "/fixture/kexec"}, o, f.d)
			if status != tc.status || (tc.eof && (err == nil || err.Error() != "EOF at final confirmation")) || (!tc.eof && err != nil) {
				t.Fatal(status, err)
			}
			want := tc.want
			if status == 0 {
				if len(f.created) != 2 || len(f.argv) != 1 {
					t.Fatal(f.created, f.argv)
				}
				want += "Downloading http://netboot.test/node.test/7/bzImage -> " + f.created[0] + "\n" +
					"Downloading http://netboot.test/node.test/7/initrd -> " + f.created[1] + "\n" +
					"Executing: /fixture/kexec --load " + f.created[0] + " --initrd=" + f.created[1] + " --append=\"httproot=http://netboot.test/node.test/7/initrd init=/fixture/store/system-7/init console=ttyS0 runlevel=rescue \" --no-sync\n" +
					"kexec --load completed successfully.\n"
			} else if len(f.requests) != 1 || len(f.created) != 0 || len(f.argv) != 0 {
				t.Fatal("effects before confirmation", f.requests, f.created, f.argv)
			}
			wantErr := ""
			if status == 1 && !tc.eof {
				wantErr = "Aborting\n"
			}
			if f.out.String() != want || f.err.String() != wantErr {
				t.Fatalf("stdout=%q want=%q stderr=%q want=%q", f.out.String(), want, f.err.String(), wantErr)
			}
		})
	}
}
