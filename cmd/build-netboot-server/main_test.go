package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vpsfreecz/confctl/internal/netboot"
)

func TestNB18ConfigBeforeLock(t *testing.T) {
	for _, input := range []string{"missing", "{", `{}`} {
		t.Run(input, func(t *testing.T) {
			d := netboot.OSDependencies()
			calls := 0
			d.FS.Read = func(string) ([]byte, error) {
				if input == "missing" {
					return nil, os.ErrNotExist
				}
				return []byte(input), nil
			}
			d.Lock = func(string) (func() error, error) { calls++; return nil, errors.New("unexpected lock") }
			var err bytes.Buffer
			if got := execute("/private/config", []string{"--help"}, d, &err); got != 1 || calls != 0 || err.Len() == 0 {
				t.Fatalf("status=%d lock=%d stderr=%q", got, calls, err.String())
			}
		})
	}
}

func TestNB02IgnoredArgvPrivateConfig(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	if err := os.Mkdir(profiles, 0700); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"coreutils":"/fixture/coreutils","syslinux":"/fixture/syslinux","tftpRoot":"` + filepath.Join(root, "tftp") + `","httpRoot":"` + filepath.Join(root, "http") + `","hostName":"test","httpUrl":"http://test","memtest":null,"isoImages":[]}`)
	for _, argv := range [][]string{nil, {"--help", "extra", "--version=opaque"}} {
		d := netboot.OSDependencies()
		d.Profiles = profiles
		d.LockFile = filepath.Join(root, "lock")
		var trace []string
		d.FS.Read = func(string) ([]byte, error) { trace = append(trace, "config"); return config, nil }
		d.Lock = func(string) (func() error, error) {
			trace = append(trace, "lock")
			return nil, errors.New("private lock refusal")
		}
		var err bytes.Buffer
		if execute("/private/config", argv, d, &err) != 1 || len(trace) != 2 || trace[0] != "config" || trace[1] != "lock" {
			t.Fatalf("trace=%v err=%s", trace, err.String())
		}
	}
}
