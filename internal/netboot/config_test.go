package netboot

import (
	"strings"
	"testing"
)

const configLiteral = `{"coreutils":"/fixture/coreutils","syslinux":"/fixture/syslinux","tftpRoot":"/fixture/tftp","httpRoot":"/fixture/http","hostName":"carrier.test","httpUrl":"http://netboot.test/prefix/","memtest":null,"isoImages":[]}`

func TestConfigPresenceAndLiteralOrder(t *testing.T) {
	c, err := ParseConfig([]byte(configLiteral))
	if err != nil {
		t.Fatal(err)
	}
	if c.Memtest != nil || len(c.ISO) != 0 {
		t.Fatal(c)
	}
	for _, key := range []string{"coreutils", "syslinux", "tftpRoot", "httpRoot", "hostName", "httpUrl", "memtest", "isoImages"} {
		v, err := parseJSON([]byte(configLiteral))
		if err != nil {
			t.Fatal(err)
		}
		o := v.(object)
		var rest object
		for _, m := range o {
			if m.key != key {
				rest = append(rest, m)
			}
		}
		b, _ := pretty(rest)
		if _, err := ParseConfig(b); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	for _, bad := range []string{`[]`, `null`, strings.Replace(configLiteral, `"memtest":null`, `"memtest":false`, 1), strings.Replace(configLiteral, `"isoImages":[]`, `"isoImages":null`, 1), strings.Replace(configLiteral, `"coreutils":"/fixture/coreutils"`, `"coreutils":null`, 1)} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	data := strings.Replace(configLiteral, `"isoImages":[]`, `"isoImages":[{"file":"/fixture/b.iso","label":""},{"file":"/fixture/a.iso","label":"A"}]`, 1)
	data = strings.Replace(data, `"memtest":null`, `"memtest":{"package":"/fixture/memtest","params":["a","b"]}`, 1)
	data = strings.TrimSuffix(data, "}") + `,"ruby":"ignored"}`
	c, err = ParseConfig([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if c.ISO[0].Name != "00-b.iso" || c.ISO[0].Label != "b.iso" || c.ISO[1].Name != "01-a.iso" || strings.Join(c.Memtest.Params, " ") != "a b" {
		t.Fatal(c)
	}
}

func TestLiteralURLJoinAndStrip(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{{"http://netboot.test/prefix/", "a/../b", "http://netboot.test/prefix/a/../b"}, {"/a/", "/b//c", "/a/b//c"}, {"http://x", "node <>&", "http://x/node <>&"}} {
		if got := join(tc.base, tc.path); got != tc.want {
			t.Fatalf("%q want %q", got, tc.want)
		}
	}
	if got := strip("\x00\t a\u00a0 \n\x00"); got != "a\u00a0" {
		t.Fatal(got)
	}
}
