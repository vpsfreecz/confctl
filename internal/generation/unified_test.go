package generation

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testHost(t *testing.T, host, id string, top *string, date string, kernel *string, current bool) *Host {
	return &Host{Host: host, Profile: "/private/profile", ID: integer(id), Toplevel: top, Date: stamp(t, date), KernelVersion: kernel, Current: current}
}
func testBuild(t *testing.T, host, name, top, date string, inputs Object, kernel *string, current bool) *Build {
	return &Build{Host: host, Name: name, Toplevel: top, Date: stamp(t, date), Inputs: inputs, InputsInfo: Object{{"role", Object{{"rev", name}}}}, KernelVersion: kernel, Current: current}
}

func TestUnifiedReplacementAndFirstMatch(t *testing.T) {
	root := t.TempDir()
	h1 := testHost(t, "a", "1", stringPointer("/same"), "2000-01-01T00:00:00Z", nil, true)
	h2 := testHost(t, "a", "2", stringPointer("/same"), "2001-01-01T00:00:00Z", stringPointer("remote2"), false)
	inputs := Object{{"a", "/a"}, {"b", "/b"}}
	reversed := Object{{"b", "/b"}, {"a", "/a"}}
	b1 := testBuild(t, "a", "build", "/same", "2020-01-01T00:00:00Z", inputs, stringPointer("local1"), true)
	b2 := testBuild(t, "a", "build", "/same", "2021-01-01T00:00:00Z", reversed, stringPointer("local2"), false)
	b2.InputsInfo = Object{{"new", Object{{"metadata", "second"}}}}
	before, _ := json.Marshal([]any{h1, h2, b1, b2})
	readOnly(t, root, func() {
		list := &UnifiedList{}
		g := list.AddHost(h1)
		if g.Inputs() != nil || g.InputsInfo() != nil || g.PresenceString() != "host" || g.CurrentString() != "host" {
			t.Fatal(g)
		}
		if list.AddHost(h2) != g || g.HostGeneration != h2 || g.ID.String() != "2" || g.Name != h1.ApproxName() || g.Date.Compare(h1.Date) != 0 || g.KernelVersion != nil || !g.Current || g.CurrentString() != "" {
			t.Fatal("remote replacement changed first fields")
		}
		if list.AddBuild(b1) != g || g.Name != "build" || g.Date.Compare(b1.Date) != 0 || g.KernelVersion != nil || g.PresenceString() != "build+host" || g.CurrentString() != "build" {
			t.Fatal("build attachment changed source fields")
		}
		if list.AddBuild(b2) != g || g.BuildGeneration != b2 || g.Date.Compare(b2.Date) != 0 || !reflect.DeepEqual(g.Inputs(), reversed) || !reflect.DeepEqual(g.InputsInfo(), b2.InputsInfo) || g.CurrentString() != "" || !g.Current {
			t.Fatal("Hash equality/replacement/current")
		}
		current, err := SelectList(list, SelectionOptions{Pattern: stringPointer("current")})
		if err != nil || len(current.Generations) != 1 || current.Generations[0] != g {
			t.Fatal("selector lost sticky current", current, err)
		}
		old, err := SelectList(list, SelectionOptions{Pattern: stringPointer("old")})
		if err != nil || len(old.Generations) != 0 {
			t.Fatal("old used attached flags", old, err)
		}
		otherName := testBuild(t, "a", "other", "/same", "1990-01-01T00:00:00Z", inputs, stringPointer("other"), false)
		otherMap := testBuild(t, "a", "build", "/same", "1980-01-01T00:00:00Z", Object{{"a", "/different"}, {"b", "/b"}}, nil, false)
		second, third := list.AddBuild(otherName), list.AddBuild(otherMap)
		h3 := testHost(t, "a", "3", stringPointer("/same"), "2002-01-01T00:00:00Z", stringPointer("remote3"), false)
		if list.AddHost(h3) != g || g.ID.String() != "3" || second.HostGeneration != nil || third.HostGeneration != nil || len(list.Generations) != 3 {
			t.Fatal("must attach to first match")
		}
		if g.Name != "build" || g.Date.Compare(b2.Date) != 0 || g.KernelVersion != nil {
			t.Fatal("late host changed build/initial fields")
		}
		otherHost := *h3
		otherHost.Host = "b"
		list.AddHost(&otherHost)
		list.AddHost(testHost(t, "a", "4", nil, "2000-01-01T00:00:00Z", nil, false))
		list.AddHost(testHost(t, "a", "5", stringPointer(""), "2000-01-01T00:00:00Z", nil, false))
		if len(list.Generations) != 6 {
			t.Fatal("host or nullable toplevel equality changed")
		}
	})
	after, _ := json.Marshal([]any{h1, h2, b1, b2})
	if !bytes.Equal(before, after) || h1.Date.Seconds().String() != "946684800" || b2.Date.Seconds().String() != "1609459200" {
		t.Fatal("mutated input records")
	}
	// The reciprocal local-first path keeps its initial kernel as well.
	list := &UnifiedList{}
	g := list.AddBuild(b1)
	list.AddBuild(b2)
	list.AddHost(h2)
	if g.KernelVersion != b1.KernelVersion || g.Toplevel == nil || *g.Toplevel != b1.Toplevel || g.CurrentString() != "" || !g.Current {
		t.Fatal(g)
	}
}

func TestUnifiedInventoryOffsets(t *testing.T) {
	list := &UnifiedList{}
	list.AddHosts(ParseHostList("a", "/p", []byte("/none\n/p-20-link;/a20;20\n/p-2-link;/a2;30"), Options{Location: time.UTC}))
	list.AddHosts(ParseHostList("b", "/p", []byte("/none\n/p-1-link;/b1;0"), Options{Location: time.UTC}))
	list.AddBuilds(&BuildList{Builds: []*Build{
		testBuild(t, "a", "local-a", "/local-a", "1900-01-01T00:00:00Z", Object{}, nil, false),
	}})
	list.AddBuilds(&BuildList{Builds: []*Build{
		testBuild(t, "b", "local-b", "/local-b", "1800-01-01T00:00:00Z", Object{}, nil, false),
	}})
	wantHosts := []string{"a", "a", "b", "a", "b"}
	for i, g := range list.Generations {
		if g.Host != wantHosts[i] {
			t.Fatal("global sorting introduced")
		}
	}
	if list.Generations[0].ID.String() != "2" || list.Generations[1].ID.String() != "20" {
		t.Fatal("host ID order changed")
	}
	for _, tc := range []struct {
		offset string
		index  int
	}{
		{"0", 4}, {"1", 0}, {"2", 1}, {"5", 4}, {"-1", 3}, {"-4", 0}, {"-5", -1}, {"6", -1}, {"9999999999999999999999999", -1}, {"-9999999999999999999999999", -1},
	} {
		got := list.AtOffset(integer(tc.offset))
		if tc.index < 0 {
			if got != nil {
				t.Fatal(tc.offset, got)
			}
		} else if got != list.Generations[tc.index] {
			t.Fatal(tc.offset, got)
		}
	}
	if (&UnifiedList{}).AtOffset(integer("0")) != nil {
		t.Fatal("empty offset")
	}
}

// A finite original method adapter uses projected fake records. It exercises
// original Unified/UnifiedList setters and membership, with no remote effects.
const originalUnifiedAdapter = `
require 'json'
require 'time'
$LOAD_PATH.unshift(ARGV.fetch(0))
module ConfCtl
  module Generation
    class Build
      attr_reader :host, :name, :toplevel, :date, :kernel_version, :current, :inputs, :inputs_info
      def initialize(f)
        @host, @name, @toplevel = f.values_at('host', 'name', 'top')
        @date = Time.iso8601(f['date'])
        @kernel_version, @current, @inputs, @inputs_info = f.values_at('kernel', 'current', 'inputs', 'info')
      end
    end
  end
end
require 'confctl/generation/host'
require 'confctl/generation/unified'
require 'confctl/generation/unified_list'
list = ConfCtl::Generation::UnifiedList.new
JSON.parse(STDIN.read).each do |f|
  if f['kind'] == 'host'
    machine = Struct.new(:name).new(f['host'])
    g = ConfCtl::Generation::Host.new(machine, '/private/profile', f['id'].to_i, f['top'], Time.iso8601(f['date']), f['kernel'], current: f['current'])
    list.add_host_generation(g)
  else
    list.add_build_generation(ConfCtl::Generation::Build.new(f))
  end
end
puts JSON.generate(list.map do |g|
  {host: g.host, name: g.name, id: g.id && g.id.to_s, top: g.toplevel,
   seconds: g.date.to_i.to_s, fraction: g.date.subsec.to_r.to_s,
   kernel: g.kernel_version, current: !!g.current, current_str: g.current_str,
   presence: g.presence_str, inputs: g.inputs, info: g.inputs_info}
end)
`

func TestOriginalRubyUnifiedMethods(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	list := &UnifiedList{}
	h1 := testHost(t, "a", "1", stringPointer("/same"), "2000-01-01T00:00:00Z", nil, true)
	h2 := testHost(t, "a", "2", stringPointer("/same"), "2001-01-01T00:00:00Z", stringPointer("later"), false)
	b1 := testBuild(t, "a", "one", "/same", "2020-01-01T00:00:00.123456789012345Z", Object{{"a", "/a"}, {"b", "/b"}}, stringPointer("local"), true)
	b2 := testBuild(t, "a", "one", "/same", "2021-01-01T00:00:00Z", Object{{"b", "/b"}, {"a", "/a"}}, stringPointer("replacement"), false)
	b3 := testBuild(t, "a", "two", "/same", "1990-01-01T00:00:00Z", b1.Inputs, nil, false)
	b4 := testBuild(t, "a", "one", "/same", "1980-01-01T00:00:00Z", Object{{"a", "/different"}}, nil, false)
	ops := []any{h1, h2, b1, b2, b3, b4, h1, testHost(t, "b", "9999999999999999999999", nil, "0000-01-01T00:00:00Z", nil, false), testHost(t, "b", "0", stringPointer(""), "0000-01-01T00:00:00Z", nil, false)}
	fixtures := []map[string]any{}
	for _, op := range ops {
		var f map[string]any
		switch g := op.(type) {
		case *Host:
			list.AddHost(g)
			f = map[string]any{"kind": "host", "host": g.Host, "id": g.ID.String(), "top": g.Toplevel, "kernel": g.KernelVersion, "current": g.Current}
			// Format exact ISO input for the finite years used here.
			f["date"] = time.Unix(g.Date.Seconds().Int64(), 0).UTC().Format("2006-01-02T15:04:05Z")
		case *Build:
			list.AddBuild(g)
			inputs := map[string]string{}
			for _, m := range g.Inputs {
				inputs[m.Name] = m.Value.(string)
			}
			info := map[string]any{"role": map[string]any{"rev": g.Name}}
			f = map[string]any{"kind": "build", "host": g.Host, "name": g.Name, "top": g.Toplevel, "date": time.Unix(g.Date.Seconds().Int64(), 0).UTC().Format("2006-01-02T15:04:05Z"), "kernel": g.KernelVersion, "current": g.Current, "inputs": inputs, "info": info}
			if g == b1 || g == b3 {
				f["inputs"] = json.RawMessage(`{"a":"/a","b":"/b"}`)
			}
			if g == b2 {
				f["inputs"] = json.RawMessage(`{"b":"/b","a":"/a"}`)
			}
			if g == b1 {
				f["date"] = "2020-01-01T00:00:00.123456789012345Z"
			}
		}
		fixtures = append(fixtures, f)
	}
	input, _ := json.Marshal(fixtures)
	root := t.TempDir()
	readOnly(t, root, func() {
		cmd := exec.Command(ruby, "-e", originalUnifiedAdapter, lib)
		cmd.Env = append(cmd.Environ(), "TZ=UTC")
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("original unified: %v %s", err, stderr.Bytes())
		}
		var rows []struct {
			Host, Name        string
			ID, Top, Kernel   *string
			Seconds, Fraction string
			Current           bool
			CurrentString     *string `json:"current_str"`
			Presence          string
			Inputs            map[string]string
			Info              map[string]map[string]string
		}
		if err := json.Unmarshal(out, &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(list.Generations) {
			t.Fatal("original unified size", rows)
		}
		for i, row := range rows {
			g := list.Generations[i]
			var id *string
			if g.ID != nil {
				id = stringPointer(g.ID.String())
			}
			current := g.CurrentString()
			var currentString *string
			if current != "" {
				currentString = &current
			}
			if row.Host != g.Host || row.Name != g.Name || !samePath(row.ID, id) || !samePath(row.Top, g.Toplevel) || !samePath(row.Kernel, g.KernelVersion) || row.Seconds != g.Date.Seconds().String() || row.Current != g.Current || !samePath(row.CurrentString, currentString) || row.Presence != g.PresenceString() {
				t.Fatalf("original%+v Go%+v", row, g)
			}
			// Ruby Rational#to_s writes integral fractions with /1.
			fraction := g.Date.Fraction()
			if row.Fraction != fraction.Num().String()+"/"+fraction.Denom().String() {
				t.Fatal("fraction changed", row.Fraction, fraction)
			}
			if g.BuildGeneration == nil {
				if row.Inputs != nil || row.Info != nil {
					t.Fatal("remote metadata")
				}
				continue
			}
			if len(row.Inputs) != len(g.Inputs()) || row.Info["role"]["rev"] != g.BuildGeneration.Name {
				t.Fatal("attached metadata")
			}
			for _, m := range g.Inputs() {
				if row.Inputs[m.Name] != m.Value.(string) {
					t.Fatal("input differs")
				}
			}
		}
	})
}
