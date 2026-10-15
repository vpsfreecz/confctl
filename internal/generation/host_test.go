package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func stringPointer(s string) *string { return &s }
func integer(s string) *big.Int      { n, _ := new(big.Int).SetString(s, 10); return n }
func stamp(t *testing.T, s string) Timestamp {
	t.Helper()
	d, err := ParseISO8601(s, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestProfileFetchContract(t *testing.T) {
	profile := "/private/carried/profile;literal"
	expected := strings.Join([]string{
		"realpath " + profile, "",
		"for generation in `ls -d -1 " + profile + "-*-link` ; do",
		"  echo -n \"$generation;\"",
		"  echo -n \"$(readlink $generation);\"",
		"  echo -n \"$(stat --format=%Y $generation);\"", "",
		"  for kernel_file in kernel bzImage ; do",
		"    [ -h \"$generation/$kernel_file\" ] && echo -n $(readlink \"$generation/$kernel_file\")",
		"  done", "", "  echo", "done", "",
	}, "\n")
	spawn := errors.New("spawn")
	wait := errors.New("wait")
	for _, tc := range []struct {
		name    string
		success bool
		err     error
		out     string
		present bool
	}{
		{"empty", true, nil, "", true},
		{"ordinary_failure", false, nil, "/some/output", false},
		{"spawn", false, spawn, "", false},
		{"wait", false, wait, "", false},
		{"cancel", true, context.Canceled, "/some/output", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), struct{}{}, "bound")
			calls := 0
			root := t.TempDir()
			readOnly(t, root, func() {
				list, err := FetchHostList(ctx, "logical/carried", profile, Options{}, func(actual context.Context, argv []string, stdin []byte) ([]byte, bool, error) {
					calls++
					if actual != ctx || !reflect.DeepEqual(argv, []string{"bash", "--norc"}) || string(stdin) != expected {
						t.Fatalf("callback changed %v %q", argv, stdin)
					}
					return []byte(tc.out), tc.success, tc.err
				})
				if err != tc.err || (list != nil) != tc.present || calls != 1 {
					t.Fatalf("list%v err%v calls%d", list, err, calls)
				}
				if list != nil && (list.Host != "logical/carried" || list.CurrentPath != nil || len(list.Generations) != 0) {
					t.Fatal(list)
				}
			})
		})
	}
}

func TestProfileLiteralParsing(t *testing.T) {
	profile := "/p.[x]"
	out := "\x00 \t/target/../system\n" +
		"/invalid;/ignored;99\n" +
		profile + "-10-link;/target/system;20;/nix/store/hash-linux-6.1/kernel/nix/store/other-linux-7/bzImage\n" +
		profile + "-2-link;/target/../system; -12_345x;/nix/store/hash-linux-5.10/bzImage;ignored\n" +
		profile + "-02-link;;0;\n" +
		profile + "-3-link;\n" +
		profile + "-922337203685477580812345-link;/huge;abc\n" +
		"/pXx-4-link;/wrong;0\r\n\x00"
	warnings := []string{}
	root := t.TempDir()
	readOnly(t, root, func() {
		list := ParseHostList("logical", profile, []byte(out), Options{Location: time.UTC, Warn: func(s string) { warnings = append(warnings, s) }})
		if list.CurrentPath == nil || *list.CurrentPath != "/target/../system" {
			t.Fatal(list.CurrentPath)
		}
		if !reflect.DeepEqual(warnings, []string{"Invalid profile generation link '/invalid'", "Invalid profile generation link '/pXx-4-link'"}) {
			t.Fatal(warnings)
		}
		ids := []string{}
		for _, g := range list.Generations {
			ids = append(ids, g.ID.String())
			if g.Host != "logical" || g.Profile != profile {
				t.Fatal(g)
			}
		}
		if !reflect.DeepEqual(ids, []string{"2", "2", "3", "10", "922337203685477580812345"}) {
			t.Fatal(ids)
		}
		g := list.Generations
		if list.Current() != g[0] || !g[0].Current || g[3].Current || g[0].Date.Seconds().String() != "-12345" || *g[0].KernelVersion != "5.10" {
			t.Fatal(g[0])
		}
		if g[1].Toplevel == nil || *g[1].Toplevel != "" || g[2].Toplevel != nil || g[1].Date.Seconds().Sign() != 0 || g[4].Date.Seconds().Sign() != 0 {
			t.Fatal("absent/empty/zero changed")
		}
		if *g[3].KernelVersion != "6.1" {
			t.Fatal(g[3].KernelVersion)
		}
	})
	for _, output := range []string{"", " \x00\t\r\n\v\f"} {
		list := ParseHostList("host", profile, []byte(output), Options{})
		if len(list.Generations) != 0 || list.CurrentPath != nil || list.Current() != nil {
			t.Fatal(list)
		}
	}
	// Unicode whitespace is not Ruby String#strip whitespace.
	list := ParseHostList("host", profile, []byte("\u00a0current\u00a0\n"+profile+"-1-link;\u00a0current\u00a0;0"), Options{})
	if *list.CurrentPath != "\u00a0current\u00a0" || list.Current() == nil {
		t.Fatal(list)
	}
}

func TestEpochCoercionAndCalendar(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "0"}, {"abc", "0"}, {"  -12_345x", "-12345"}, {"1__2", "1"}, {"1_", "1"}, {"_1", "0"}, {"+0012junk", "12"}, {"0x10", "0"}, {"\x0012", "0"}, {"\v-12", "-12"}, {"12\x00x", "12"}, {"999999999999999999999999999999", "999999999999999999999999999999"},
	} {
		if got := rubyEpochInteger(tc.input).String(); got != tc.want {
			t.Fatalf("%q -> %s, want %s", tc.input, got, tc.want)
		}
	}
	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ seconds, utc, local string }{
		{"-1", "1969-12-31--23-59-59", "1970-01-01--00-59-59"},
		{"-62198755200", "-0001-01-01--00-00-00", "-0001-01-01--00-17-30"},
		{"-62167219200", "0000-01-01--00-00-00", "0000-01-01--00-17-30"},
		{"253402300800", "10000-01-01--00-00-00", "10000-01-01--01-00-00"},
		{"3155695199999999937832786200", "100000000000000000000-01-01--01-30-00", "100000000000000000000-01-01--02-30-00"},
	} {
		for _, zone := range []struct {
			location *time.Location
			want     string
		}{{time.UTC, tc.utc}, {amsterdam, tc.local}} {
			g := &Host{Date: epochTimestamp(integer(tc.seconds), zone.location)}
			if g.ApproxName() != zone.want || g.Date.Seconds().String() != tc.seconds {
				t.Fatalf("%s %s -> %s want%s", tc.seconds, zone.location, g.ApproxName(), zone.want)
			}
		}
	}
	// Calendar inversion across signed years and leap/month boundaries.
	for _, year := range []string{"-100000000000000000001", "-400", "-1", "0", "1", "1900", "2000", "2400", "100000000000000000000"} {
		for month := 1; month <= 12; month++ {
			y := integer(year)
			d := monthDays(y, month)
			seconds := calendarSeconds(y, month, d, 23, 59, 59)
			gy, gm, gd, gh, gmin, gs := epochCalendar(seconds)
			if gy.Cmp(y) != 0 || gm != month || gd != d || gh != 23 || gmin != 59 || gs != 59 {
				t.Fatal(year, month, gy, gm, gd, gh, gmin, gs)
			}
		}
	}
}

// Optional ORIGINAL-ADAPTER: fake machine control returns literal bytes and
// records the script; it never executes the shell or queries a profile.
const originalHostAdapter = `
require 'json'
$LOAD_PATH.unshift(ARGV.fetch(0))
module ConfCtl; module Generation; end; end
require 'confctl/generation/host'
require 'confctl/generation/host_list'
class FakeControl
  attr_reader :script
  def initialize(output); @output = output; end
  def bash_script(script); @script = script; [@output, '']; end
end
machine = Struct.new(:name).new('logical')
puts JSON.generate(JSON.parse(STDIN.read).map do |f|
  mc = FakeControl.new(f['output'])
  list = ConfCtl::Generation::HostList.fetch(machine, mc, profile: f['profile'])
  rows = []
  list.each do |g|
    rows << {id: g.id.to_s, top: g.toplevel, seconds: g.date.to_i.to_s,
      name: g.approx_name, offset: g.date.utc_offset, kernel: g.kernel_version,
      current: g.current}
  end
  {script: mc.script, rows: rows, current: list.current && list.current.id.to_s}
end)
`

func TestOriginalRubyHostParser(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	profile := "/private/p.[x]"
	fixtures := []map[string]string{
		{"profile": profile, "output": ""},
		{"profile": profile, "output": "/system\n" + profile + "-2-link;/system; -12_345x;/nix/store/hash-linux-6.1/kernel/nix/store/other-linux-7/bzImage;surplus\n" + profile + "-01-link;;0;\n" + profile + "-1-link;\ninvalid;/x;0\n" + profile + "-999999999999999999999-link;/later;+0012junk"},
		{"profile": profile, "output": "\x00 \t/zero\n" + profile + "-1-link;/zero;-62167219200\r\n\x00"},
		{"profile": profile, "output": "/huge\n" + profile + "-1-link;/huge;3155695199999999937832786200"},
		{"profile": profile, "output": "\u00a0current\u00a0\n" + profile + "-1-link;\u00a0current\u00a0;1__2;"},
	}
	input, _ := json.Marshal(fixtures)
	for _, zone := range []string{"UTC", "Europe/Amsterdam"} {
		t.Run(zone, func(t *testing.T) {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			readOnly(t, root, func() {
				cmd := exec.Command(ruby, "-e", originalHostAdapter, lib)
				cmd.Env = append(cmd.Environ(), "TZ="+zone)
				cmd.Stdin = bytes.NewReader(input)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("original host: %v %s", err, stderr.Bytes())
				}
				var results []struct {
					Script  string
					Current *string
					Rows    []struct {
						ID            string
						Top           *string
						Seconds, Name string
						Offset        int
						Kernel        *string
						Current       bool
					}
				}
				if err := json.Unmarshal(out, &results); err != nil {
					t.Fatal(err)
				}
				if len(results) != len(fixtures) {
					t.Fatal("missing adapter output")
				}
				warnings := []string{}
				for i, r := range results {
					f := fixtures[i]
					list := ParseHostList("logical", f["profile"], []byte(f["output"]), Options{Location: location, Warn: func(s string) { warnings = append(warnings, s) }})
					if r.Script != string(ProfileScript(f["profile"])) || len(r.Rows) != len(list.Generations) {
						t.Fatal("script/rows differ", r)
					}
					for j, row := range r.Rows {
						g := list.Generations[j]
						if row.ID != g.ID.String() || !samePath(row.Top, g.Toplevel) || row.Seconds != g.Date.Seconds().String() || row.Name != g.ApproxName() || row.Offset != g.Date.OffsetSeconds || !samePath(row.Kernel, g.KernelVersion) || row.Current != g.Current {
							t.Fatalf("fixture%d row%d: original%+v Go%+v %s", i, j, row, g, g.ApproxName())
						}
					}
					current := list.Current()
					if (r.Current == nil) != (current == nil) || current != nil && *r.Current != current.ID.String() {
						t.Fatal("current differs")
					}
				}
				if strings.TrimSuffix(stderr.String(), "\n") != strings.Join(warnings, "\n") {
					t.Fatalf("warnings original%q Go%v", stderr.String(), warnings)
				}
			})
		})
	}
}
