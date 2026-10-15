package netboot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is the literal synthetic model used by the isolated original-method
// adapter. Its /fixture paths are wire values, never host filesystem inputs.
func originalModel(t *testing.T) (Config, []*Machine) {
	t.Helper()
	c := Config{Coreutils: "/fixture/tools/coreutils", Syslinux: "/fixture/tools/syslinux", TFTPRoot: "/fixture/roots/tftp", HTTPRoot: "/fixture/roots/http", HostName: "carrier.test", HTTPURL: "http://netboot.test", Memtest: &Memtest{"/fixture/tools/memtest", []string{"console=ttyS0", "fixture-param"}}, ISO: []ISOImage{{"/fixture/images/a.iso", "a.iso", "00-a.iso"}, {"/fixture/images/b.iso", "Second ISO", "01-b.iso"}}}
	inputs, err := parseJSON([]byte(`{"z":"ž <>& \u2028","a":[9007199254740993123456789,1.2,1e-7,1e15],"nested":{"b":false,"a":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	ms := []*Machine{{Name: "rack:node", Spin: "nixos", FQDN: "node.test", Label: "Node label\nLABEL extra"}, {Name: "aaa-other", Spin: "vpsadminos", FQDN: "other.test", Label: "other.test.example", ShortLabel: "other.test"}}
	for _, m := range ms {
		m.URL = "http://netboot.test/" + m.FQDN
		ids, current := []int64{7, 6, 5}, int64(6)
		if m.Spin == "vpsadminos" {
			ids, current = []int64{1}, 1
		}
		for _, id := range ids {
			g := &Generation{ID: big.NewInt(id), Store: fmt.Sprintf("/fixture/store/netboot-%d", id), Toplevel: fmt.Sprintf("/fixture/store/system-%d", id), Time: time.Unix(1727740800+id, 0), TimeS: fmt.Sprintf("2024-10-01 00:00:%02d", id), Current: id == current, Version: "24.11.20241001", Revision: "0123456789abcdef", ShortRev: "012345678", KernelVersion: "6.6.52", MACs: []any{"AA:BB:CC:DD:EE:FF"}, Inputs: inputs, Params: []string{"console=ttyS0"}, URL: fmt.Sprintf("%s/%d", m.URL, id)}
			names := []string{"bzImage", "initrd"}
			if m.Spin == "vpsadminos" {
				names = append(names, "root.squashfs")
			}
			for _, n := range names {
				g.Boot = append(g.Boot, BootFile{n, "/fixture/assets/" + n, g.URL + "/" + n})
			}
			// Resolve the private model with a literal path adapter, exercising the
			// real params/variant implementation without following /fixture paths.
			boot := g.Boot
			g.Boot = nil
			d := Dependencies{FS: Filesystem{Realpath: func(p string) (string, error) {
				for _, b := range boot {
					if filepath.Base(p) == b.Name {
						return b.Path, nil
					}
				}
				return "", os.ErrNotExist
			}}}
			if err := resolveGeneration(m, g, d); err != nil {
				t.Fatal(err)
			}
			m.Generations = append(m.Generations, g)
			if g.Current {
				m.Current = g
			}
		}
	}
	return c, ms
}

func deepInput(shape string, n int) any {
	var v any = json.Number("0")
	if strings.HasSuffix(shape, "empty") {
		if strings.HasPrefix(shape, "array") {
			v = []any{}
		} else {
			v = object{}
		}
	}
	for i := 0; i < n; i++ {
		if strings.HasPrefix(shape, "array") {
			v = []any{v}
		} else {
			v = object{{"a", v}}
		}
	}
	return v
}

func TestSelectedProviderWrappedGeneratorDepth(t *testing.T) {
	// Observed with actual original Generation/Machine#to_json(state). Their
	// delegate hashes retain state depth; empty deepest containers count too.
	for _, shape := range []string{"array_scalar", "array_empty", "object_scalar", "object_empty"} {
		for n := 93; n <= 99; n++ {
			empty := strings.HasSuffix(shape, "empty")
			if empty && n == 99 {
				continue
			} // not admitted by original metadata reader
			t.Run(fmt.Sprintf("%s_%d", shape, n), func(t *testing.T) {
				_, ms := originalModel(t)
				g := ms[0].Current
				g.Inputs = deepInput(shape, n)
				for _, out := range []struct {
					name  string
					value any
					max   int
				}{{"generation", generationRecord(g), 99}, {"machine", machineRecord(ms[0]), 97}, {"machines", object{{"machines", []any{machineRecord(ms[0])}}}, 95}} {
					max := out.max
					if empty {
						max--
					}
					_, err := pretty(out.value)
					if (err == nil) != (n <= max) {
						t.Fatalf("%s %s %d error=%v source maximum=%d", out.name, shape, n, err, max)
					}
				}
			})
		}
	}
}

func TestWrappedGeneratorDepthFailureAtActualHTTPAccess(t *testing.T) {
	for _, tc := range []struct {
		wrappers  int
		empty     bool
		atMachine bool
	}{{98, false, true}, {97, true, true}, {96, false, false}, {95, true, false}} {
		t.Run(fmt.Sprintf("%d_%v", tc.wrappers, tc.empty), func(t *testing.T) {
			f := makeFixture(t)
			oldT := f.old(f.c.TFTPRoot, "123abc")
			oldH := f.old(f.c.HTTPRoot, "456def")
			payload := strings.Repeat("[", tc.wrappers) + "0" + strings.Repeat("]", tc.wrappers)
			if tc.empty {
				payload = strings.Repeat("[", tc.wrappers) + "[]" + strings.Repeat("]", tc.wrappers)
			}
			orig := f.d.FS.Read
			f.d.FS.Read = func(p string) ([]byte, error) {
				b, err := orig(p)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(p, "confctl-rack:node-7-link/machine.json") {
					b = append(append(b[:len(b)-1], ','), []byte(`"inputs-info":`+payload+`}`)...)
				}
				return b, nil
			}
			err := Run(f.c, f.d)
			requireError(t, err, "JSON output nesting exceeds 100")
			if !reflect.DeepEqual(f.trace, []string{"lock", "profiles", "suffix", "unlock"}) {
				t.Fatal("moved output failure/effects", f.trace)
			}
			requireLink(t, f.c.TFTPRoot, "tftp.123abc")
			requireLink(t, f.c.HTTPRoot, "http.456def")
			for _, p := range []string{oldT + "/old", oldH + "/old"} {
				b, err := os.ReadFile(p)
				if err != nil || string(b) != "old sentinel" {
					t.Fatal("old root changed", p, err)
				}
			}
			http := f.c.HTTPRoot + ".abcdef"
			requireLink(t, http+"/node.test/current", "7")
			for _, id := range []string{"7", "6", "5"} {
				for _, name := range []string{"generation.json", "kernel-params"} {
					if _, err := os.ReadFile(http + "/node.test/" + id + "/" + name); err != nil {
						t.Fatal("earlier generation output missing", err)
					}
				}
			}
			requireAbsent(t, http+"/machines.json")
			if tc.atMachine {
				requireAbsent(t, http+"/node.test/machine.json")
			} else {
				if _, err := os.ReadFile(http + "/node.test/machine.json"); err != nil {
					t.Fatal("earlier machine output missing", err)
				}
			}
			if _, err := os.ReadFile(f.c.TFTPRoot + ".abcdef/pxeserver/machines/node.test/auto.cfg"); err != nil {
				t.Fatal("TFTP incomplete before HTTP access", err)
			}
		})
	}
}

func TestJSONAdmissionDepthAndIncompleteHighSurrogates(t *testing.T) {
	for _, data := range []string{`{"fqdn":"x\ud800"}`, `{"ignored":"x\ud800a"}`, `{"\ud800":0}`, strings.Repeat("[", 101) + "0" + strings.Repeat("]", 101), strings.Repeat("[", 101) + strings.Repeat("]", 101), strings.Repeat(`{"a":`, 100) + `{}` + strings.Repeat("}", 100)} {
		if _, err := parseJSON([]byte(data)); err == nil {
			t.Fatalf("admitted corrupt/deep data %q", data)
		}
	}
	for _, data := range []string{`{"fqdn":"x\ud800\udc00"}`, `{"fqdn":"x\ufffd"}`, `{"fqdn":"x�"}`, `{"ignored":"\\ud800"}`, `{"ignored":"\udc00"}`, strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100), strings.Repeat("[", 100) + strings.Repeat("]", 100), strings.Repeat(`{"a":`, 99) + `{}` + strings.Repeat("}", 99)} {
		if _, err := parseJSON([]byte(data)); err != nil {
			t.Fatalf("refused supported boundary %q: %v", data, err)
		}
	}
}

func TestJSONAdmissionRefusesBeforeCandidateEffects(t *testing.T) {
	for _, bad := range []string{`"fqdn":"node\ud800"`, `"ignored":"\ud800"`, `"ignored":` + strings.Repeat("[", 100) + "0" + strings.Repeat("]", 100), `"ignored":` + strings.Repeat(`{"a":`, 99) + `{}` + strings.Repeat("}", 99)} {
		t.Run(bad[:8], func(t *testing.T) {
			f := makeFixture(t)
			oldT := f.old(f.c.TFTPRoot, "123abc")
			oldH := f.old(f.c.HTTPRoot, "456def")
			beforeT, _ := os.ReadFile(oldT + "/old")
			beforeH, _ := os.ReadFile(oldH + "/old")
			iT, _ := os.Lstat(oldT)
			iH, _ := os.Lstat(oldH)
			orig := f.d.FS.Read
			f.d.FS.Read = func(p string) ([]byte, error) {
				if strings.HasSuffix(p, "confctl-rack:node-7-link/machine.json") {
					top := filepath.Join(f.root, "system/rack:node-7")
					return []byte(`{"toplevel":` + quote(top) + `,` + bad + `}`), nil
				}
				return orig(p)
			}
			if err := Run(f.c, f.d); err == nil {
				t.Fatal("invalid profile reached build")
			}
			requireAbsent(t, f.c.TFTPRoot+".abcdef")
			requireAbsent(t, f.c.HTTPRoot+".abcdef")
			requireLink(t, f.c.TFTPRoot, "tftp.123abc")
			requireLink(t, f.c.HTTPRoot, "http.456def")
			aT, err := os.ReadFile(oldT + "/old")
			if err != nil || !bytes.Equal(aT, beforeT) {
				t.Fatal("old TFTP changed", err)
			}
			aH, err := os.ReadFile(oldH + "/old")
			if err != nil || !bytes.Equal(aH, beforeH) {
				t.Fatal("old HTTP changed", err)
			}
			jT, _ := os.Lstat(oldT)
			jH, _ := os.Lstat(oldH)
			if !os.SameFile(iT, jT) || !os.SameFile(iH, jH) {
				t.Fatal("old tree identity changed")
			}
			if f.out.Len() != 0 || !reflect.DeepEqual(f.trace, []string{"lock", "profiles", "unlock"}) {
				t.Fatalf("effects %v stdout %q", f.trace, f.out.String())
			}
		})
	}
}

func TestJSONSelectedProviderSurrogateBytes(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`x\ud800\udc00`, "x\U00010000"},
		{`x\ud800\u0041`, "x\U00010041"},
		{`x\udc00`, "x\xed\xb0\x80"},
		{`x\udc00\udfff`, "x\xed\xb0\x80\xed\xbf\xbf"},
		{`x\ufffd`, "x�"}, {"x�", "x�"},
		{`x\\ud800`, `x\ud800`},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			v, err := parseJSON([]byte(`{"value":"` + tc.raw + `"}`))
			if err != nil {
				t.Fatal(err)
			}
			got, _ := v.(object).get("value")
			if got != tc.want {
				t.Fatalf("bytes=%x want %x", got, tc.want)
			}
			_, err = pretty(v)
			if strings.Contains(tc.want, "\xed") {
				if err == nil {
					t.Fatal("invalid source bytes became valid output")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	// A lone-low object key and legitimate replacement character remain distinct.
	v, err := parseJSON([]byte(`{"\udc00":1,"�":2}`))
	if err != nil {
		t.Fatal(err)
	}
	o := v.(object)
	if len(o) != 2 || o[0].key != "\xed\xb0\x80" || o[1].key != "�" {
		t.Fatalf("keys=%#v", o)
	}
	if _, err := pretty(v); err == nil {
		t.Fatal("invalid key emitted")
	}
}

func TestJSONLowSurrogateFailureAtPrettyAccessAndIgnoredAdmission(t *testing.T) {
	for _, carried := range []bool{false, true} {
		t.Run(fmt.Sprintf("carried_%v", carried), func(t *testing.T) {
			f := makeFixture(t)
			oldT := f.old(f.c.TFTPRoot, "123abc")
			oldH := f.old(f.c.HTTPRoot, "456def")
			orig := f.d.FS.Read
			f.d.FS.Read = func(p string) ([]byte, error) {
				b, err := orig(p)
				if err != nil {
					return nil, err
				}
				if strings.HasSuffix(p, "confctl-rack:node-7-link/machine.json") {
					field := `"ignored":"\udc00"`
					if carried {
						field = `"inputs-info":{"low":"\udc00"}`
					}
					b = append(append(b[:len(b)-1], ','), []byte(field+`}`)...)
				}
				return b, nil
			}
			err := Run(f.c, f.d)
			if !carried {
				if err != nil {
					t.Fatal(err)
				}
				requireLink(t, f.c.TFTPRoot, "tftp.abcdef")
				requireLink(t, f.c.HTTPRoot, "http.abcdef")
				return
			}
			requireError(t, err, "invalid UTF-8 string in netboot index output")
			wantTrace := []string{"lock", "profiles", "suffix", "unlock"}
			if !reflect.DeepEqual(f.trace, wantTrace) {
				t.Fatalf("effects=%v", f.trace)
			}
			if f.out.Len() == 0 {
				t.Fatal("failure moved before source console output")
			}
			requireLink(t, f.c.TFTPRoot, "tftp.123abc")
			requireLink(t, f.c.HTTPRoot, "http.456def")
			for _, p := range []string{oldT + "/old", oldH + "/old"} {
				b, err := os.ReadFile(p)
				if err != nil || string(b) != "old sentinel" {
					t.Fatalf("old effect %s %q %v", p, b, err)
				}
			}
			if _, err := os.ReadFile(f.c.TFTPRoot + ".abcdef/pxeserver/machines/node.test/auto.cfg"); err != nil {
				t.Fatal("TFTP not completed before pretty failure", err)
			}
			checkTree(t, f.c.HTTPRoot+".abcdef", []string{"node.test", "node.test/7", "node.test/7/bzImage", "node.test/7/initrd", "node.test/7/root.squashfs"})
			requireAbsent(t, f.c.HTTPRoot+".abcdef/node.test/7/generation.json")
		})
	}
}

func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata/original", name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s bytes differ\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

func TestOriginalIndexBytes(t *testing.T) {
	_, ms := originalModel(t)
	a := []any{}
	for _, m := range ms {
		a = append(a, machineRecord(m))
	}
	b, err := pretty(object{{"machines", a}})
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "machines.json", b)
	for _, m := range ms {
		b, err = pretty(machineRecord(m))
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, m.Spin+"-machine.json", b)
		b, err = pretty(generationRecord(m.Current))
		if err != nil {
			t.Fatal(err)
		}
		assertGolden(t, m.Spin+"-generation.json", b)
	}
}

func TestOriginalScalarBytes(t *testing.T) {
	for _, name := range []string{"numbers", "more-numbers"} {
		t.Run(name, func(t *testing.T) {
			input, err := os.ReadFile("testdata/original/" + name + ".input.json")
			if err != nil {
				t.Fatal(err)
			}
			v, err := parseJSON(input)
			if err != nil {
				t.Fatal(err)
			}
			b, err := pretty(v)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, name+".json", b)
		})
	}
}

func TestIndexOrderedDuplicateKeysAndInputsPresence(t *testing.T) {
	v, err := parseJSON([]byte(`{"z":1,"a":2,"z":3}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := pretty(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\n  \"z\": 3,\n  \"a\": 2\n}" {
		t.Fatal(string(b))
	}
	_, ms := originalModel(t)
	g := ms[0].Current
	for _, v := range []any{nil, false, object{}, []any{}} {
		g.Inputs = v
		_, ok := generationRecord(g).get("inputs_info")
		if ok != truthy(v) {
			t.Fatalf("inputs=%#v present=%v", v, ok)
		}
	}
	for _, bad := range []string{`{} {}`, `{"a":}`, "\xff"} {
		if _, err := parseJSON([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if !reflect.DeepEqual(stringsValue(nil), []any{}) {
		t.Fatal("empty wire arrays must be []")
	}
}
