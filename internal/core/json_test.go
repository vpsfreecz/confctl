package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	ext "github.com/vpsfreecz/confctl/extension"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNixLiteralSDKHelper(t *testing.T) {
	if os.Getenv("CONFCTL_NIX_LITERAL_SDK_HELPER") != "1" {
		return
	}
	ext.Serve(map[string]ext.Handler{"literal": func(ctx context.Context, client *ext.Client, in ext.Invocation) error {
		value := in.Options["value"]
		if in.Options["bad-number"] == true {
			// Invalid tokens cannot be put in a valid JSON request. This exercises
			// the real Client.Nix/Peer marshaling refusal, before the service.
			value = json.Number("not-a-number")
		}
		text, err := client.Nix(ctx, value)
		if reason, wantsError := in.Options["error"].(string); wantsError {
			if err == nil || text != "" || !strings.Contains(err.Error(), reason) {
				return fmt.Errorf("expected %s error/empty result: text=%q err=%v", reason, text, err)
			}
			return nil
		}
		if err != nil || text != in.Options["want"] {
			return fmt.Errorf("SDK literal: text=%q err=%v want=%q", text, err, in.Options["want"])
		}
		return nil
	}}, []string{"format.nix"})
}

func TestNixLiteralSDKServiceCorrections(t *testing.T) {
	t.Setenv("CONFCTL_NIX_LITERAL_SDK_HELPER", "1")
	for _, options := range []map[string]any{
		{"value": map[string]any{"if": []any{"\b\f\x1b\u2028😀", json.Number("9007199254740993"), json.Number("-1"), json.Number("2"), json.Number("1e3")}},
			"want": "{\n  \"if\" = [\n    \"\b\f\x1b\u2028😀\"\n    9007199254740993\n    (-1)\n    2\n    1.0e3\n  ];\n}"},
		{"value": map[string]any{"x${key}": []any{`${value}`, json.Number("-125E-3")}},
			"want": "{\n  \"x\\${key}\" = [\n    \"\\${value}\"\n    (-125.0E-3)\n  ];\n}"},
		{"value": map[string]any{"nested": "\x00"}, "error": "NUL"},
		{"value": map[string]any{"nested": map[string]any{"\x00": true}}, "error": "NUL"},
		{"bad-number": true, "error": "invalid number literal"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		engine := &Engine{Root: t.TempDir(), Context: ctx}
		reg := Registration{ID: "test.literal", Protocol: ext.Version{Major: 1}, Argv: []string{os.Args[0], "-test.run=^TestNixLiteralSDKHelper$"}}
		code, err := engine.Invoke(reg, "literal", ext.Invocation{Root: engine.Root, Options: options})
		cancel()
		if err != nil || code != 0 {
			t.Fatalf("options=%#v code=%d err=%v", options, code, err)
		}
	}
}

func TestSettingsNixHelper(t *testing.T) {
	if os.Getenv("CORE_SETTINGS_NIX_HELPER") != "1" {
		return
	}
	f, err := os.OpenFile(os.Getenv("CORE_SETTINGS_NIX_CALLS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	for i, arg := range os.Args {
		if arg == "--" {
			err = json.NewEncoder(f).Encode(os.Args[i+1:])
			break
		}
	}
	_ = f.Close()
	if err != nil {
		os.Exit(2)
	}
	_, _ = io.WriteString(os.Stdout, os.Getenv("CORE_SETTINGS_NIX_JSON"))
	os.Exit(0)
}

func TestNumericServiceBoundaries(t *testing.T) {
	for _, jobs := range []string{"8", `"auto"`} {
		t.Run(jobs, func(t *testing.T) {
			root := t.TempDir()
			sh, err := exec.LookPath("sh")
			if err != nil {
				t.Fatal(err)
			}
			launcher := "#!" + sh + "\nexec " + ShellJoin([]string{os.Args[0], "-test.run=^TestSettingsNixHelper$", "--"}) + " \"$@\"\n"
			if err = os.WriteFile(filepath.Join(root, "nix"), []byte(launcher), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(root, "flake.nix"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CORE_SETTINGS_NIX_HELPER", "1")
			callsPath := filepath.Join(root, "calls.jsonl")
			t.Setenv("CORE_SETTINGS_NIX_CALLS", callsPath)
			t.Setenv("CORE_SETTINGS_NIX_JSON", `{"_module":{},"nix":{"maxJobs":`+jobs+`,"impureEval":false},"list":{"columns":["name"]},"custom":{"_module":{},"large":9007199254740993,"decimal":1.0,"signedMax":9223372036854775807}}`)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			engine := &Engine{Root: root, Context: ctx}
			service := &supervisor{e: engine}
			serverRead, clientWrite := io.Pipe()
			clientRead, serverWrite := io.Pipe()
			server := ext.NewPeer(ctx, serverRead, serverWrite, "c")
			client := ext.NewPeer(ctx, clientRead, clientWrite, "e")
			server.SetHandler(service.handle)
			client.SetHandler(nil)
			defer func() {
				cancel()
				_ = serverRead.Close()
				_ = serverWrite.Close()
				_ = clientRead.Close()
				_ = clientWrite.Close()
				server.WaitHandlers()
				client.WaitHandlers()
			}()

			var settings map[string]any
			if err = client.Call(ctx, "settings.get", map[string]any{}, &settings); err != nil {
				t.Fatal(err)
			}
			custom := nested(settings, "custom").(map[string]any)
			wantCustom := map[string]any{"large": json.Number("9007199254740993"), "decimal": json.Number("1.0"), "signedMax": json.Number("9223372036854775807")}
			if !reflect.DeepEqual(custom, wantCustom) || nested(settings, "nix", "impureEval") != false || settings["_module"] != nil || !reflect.DeepEqual(nested(settings, "list", "columns"), []any{"name"}) {
				t.Fatal("settings metadata or stock options changed", settings)
			}
			n, err := engine.evaluator()
			if err != nil {
				t.Fatal(err)
			}
			if err = n.argsSettings(); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(callsPath)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(bytes.TrimSpace(calls), []byte{'\n'})
			if len(lines) != 2 {
				t.Fatal("unexpected evaluator calls", string(calls))
			}
			for i, line := range lines {
				var argv []string
				if err = json.Unmarshal(line, &argv); err != nil {
					t.Fatal(err)
				}
				max := "auto"
				if i == 1 {
					max = strings.Trim(jobs, `"`)
				}
				want := []string{"eval", "--json", "--no-write-lock-file", "--no-update-lock-file", "--option", "max-jobs", max, ".#confctl.settings"}
				if !reflect.DeepEqual(argv, want) {
					t.Fatal("max-jobs or Nix options changed", argv, want)
				}
			}

			var formatted struct {
				Text string `json:"text"`
			}
			if err = client.Call(ctx, "format.nix", map[string]any{"value": custom}, &formatted); err != nil {
				t.Fatal(err)
			}
			wantNix := "{\n  \"decimal\" = 1.0;\n  \"large\" = 9007199254740993;\n  \"signedMax\" = 9223372036854775807;\n}"
			if formatted.Text != wantNix {
				t.Fatal("Nix numeric literal lost precision/type", formatted.Text)
			}

			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout := os.Stdout
			os.Stdout = write
			var response map[string]any
			err = client.Call(ctx, "ui.table", map[string]any{"columns": []map[string]any{{"key": "large", "label": "INTEGER"}, {"key": "decimal", "label": "DECIMAL"}, {"key": "signedMax", "label": "LIMIT"}}, "rows": []map[string]any{custom}, "separator": " ", "header": true}, &response)
			os.Stdout = stdout
			_ = write.Close()
			text, readErr := io.ReadAll(read)
			_ = read.Close()
			if err != nil || readErr != nil || string(text) != "INTEGER DECIMAL LIMIT\n9007199254740993 1.0 9223372036854775807\n" {
				t.Fatal("table numeric conversion changed", string(text), err, readErr)
			}
			if _, err = service.handle(ctx, "format.nix", json.RawMessage(`{"value":1}{"value":2}`)); err == nil {
				t.Fatal("accepted multiple JSON request values")
			}
		})
	}
}
