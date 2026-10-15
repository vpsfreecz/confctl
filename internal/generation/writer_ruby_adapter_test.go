package generation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These separately selected ORIGINAL-ADAPTER cases are finite private source
// composition, not packaged CLI observations or a normal Go/Ruby dependency.
func writerRubyCommand(t *testing.T, name, script, root string, args ...string) ([]byte, []byte) {
	t.Helper()
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	argv := append([]string{ruby, "-e", script, lib, root}, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	for _, env := range os.Environ() {
		key := strings.SplitN(env, "=", 2)[0]
		if key != "TZ" && key != "RUBYOPT" {
			cmd.Env = append(cmd.Env, env)
		}
	}
	cmd.Env = append(cmd.Env, "TZ=UTC")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	status := -1
	if cmd.ProcessState != nil {
		status = cmd.ProcessState.ExitCode()
	}
	var sources []map[string]any
	for _, path := range []string{ruby, filepath.Join(lib, "confctl/generation/build.rb"), filepath.Join(lib, "confctl/generation/build_list.rb"), filepath.Join(lib, "confctl/gcroot.rb"), filepath.Join(lib, "confctl/utils/file.rb"), filepath.Join(lib, "confctl/conf_dir.rb")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		sources = append(sources, map[string]any{"path": path, "bytes": len(data), "mode": fmt.Sprintf("%04o", info.Mode().Perm()), "sha256": hex.EncodeToString(sum[:])})
	}
	if evidence := os.Getenv("CONFCTL_GENERATION_WRITER_EVIDENCE"); evidence != "" {
		if !filepath.IsAbs(evidence) {
			t.Fatal("writer evidence root must be absolute")
		}
		if _, err := os.Lstat(evidence); err == nil {
			info, err := os.Lstat(evidence)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				t.Fatal("invalid evidence root")
			}
		} else if os.IsNotExist(err) {
			if err := os.Mkdir(evidence, 0700); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Fatal(err)
		}
		dir := evidence + "/" + name
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal("evidence case must be absent", err)
		}
		scriptHash := sha256.Sum256([]byte(script))
		receipt := map[string]any{"name": name, "argv": argv, "cwd": root, "TZ": "UTC", "status": status, "sources": sources, "script_sha256": hex.EncodeToString(scriptHash[:]), "limits": "Pinned original source with private ConfDir/GCRoot and finite reader/failure seams; no installed CLI, Nix child, activation, real GC root or production login lookup."}
		data, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		for path, content := range map[string][]byte{"receipt.json": append(data, '\n'), "stdout.json": stdout.Bytes(), "stderr.txt": stderr.Bytes(), "adapter.rb": []byte(script)} {
			if err := os.WriteFile(dir+"/"+path, content, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if runErr != nil || status != 0 {
		t.Fatalf("original writer adapter %s: status %d error %v\nstdout:%s\nstderr:%s", name, status, runErr, stdout.Bytes(), stderr.Bytes())
	}
	return stdout.Bytes(), stderr.Bytes()
}

const writerOriginalReader = `
gem 'json', '2.21.2'
require 'json'
require 'time'
abort 'expected json2.21.2' unless JSON::VERSION == '2.21.2'
$LOAD_PATH.unshift(ARGV.fetch(0))
root = ARGV.fetch(1)
module ConfCtl
  module Generation; end
  module Utils; end
  class Error < StandardError; end
  class ConfDir
    class << self; attr_accessor :generation_dir; end
  end
  class Logger
    def self.instance; self; end
    def self.open?; false; end
  end
  def self.safe_host_name(host); host.gsub('/', ':'); end
end
require 'confctl/generation/build'
require 'confctl/gcroot'
require 'confctl/generation/build_list'
ConfCtl::ConfDir.generation_dir = File.join(root, '.confctl', 'generations')
ConfCtl::GCRoot.define_singleton_method(:dir) { File.join(root, 'private-gcroots') }
list = ConfCtl::Generation::BuildList.new('nested/host')
numeric = begin
  {name: list.at_offset(0)&.name}
rescue ConfCtl::Error => e
  {error: e.message}
end
records = list.to_a.map do |gen|
  cfg = JSON.parse(File.binread(File.join(gen.dir, 'generation.json')))
  info = gen.inputs_info
  {name: gen.name, seconds: gen.date.to_i.to_s, fraction: gen.date.subsec.to_r.to_s,
   utc: gen.date.utc?, offset: gen.date.utc_offset, fields: cfg.keys,
   input_roles: gen.inputs.keys, info_roles: info.keys, kernel: gen.kernel_version,
   links: Dir.children(gen.dir).select { |v| File.symlink?(File.join(gen.dir, v)) }.sort.to_h { |v| [v, File.readlink(File.join(gen.dir, v))] },
   large: info.dig('z', 'large')&.to_s, large_type: info.dig('z', 'large')&.class&.name,
   huge: info.dig('z', 'nested')&.first&.fetch('huge', nil)&.to_s,
   floating: info.dig('z', 'nested')&.first&.fetch('floating', nil)&.class&.name}
end
puts JSON.generate(ruby: RUBY_VERSION, json: JSON::VERSION,
  json_provider: $LOADED_FEATURES.find { |v| v.end_with?('/json/common.rb') },
  current: list.select_current!&.name, records: records,
  numeric: numeric, rejected: list.rejected.keys,
  root_links: Dir.children(ConfCtl::GCRoot.dir).sort.to_h { |v| [v, File.readlink(File.join(ConfCtl::GCRoot.dir, v))] })
`

func TestWriterOriginalRubyReader(t *testing.T) {
	_ = rubyAdapter(t) // Skip only the separate optional lane before constructing.
	root, roots, w, unused := writerFixture(t)
	_ = unused
	if err := os.Mkdir(root+"/system", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/hash-linux-6.12.3/bzImage", root+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	rollback := "/fake/rollback"
	inputs := Object{{"z", "/fake/z"}, {"a", "/fake/a"}}
	info := Object{{"z", Object{{"large", json.Number("9007199254740993")}, {"nested", []any{Object{{"huge", json.Number("999999999999999999999999999999")}, {"floating", float64(1)}, {"null", nil}}}}}}, {"a", "Unicode žluť & <literal>"}}
	var builds []*Build
	for i, zone := range []*time.Location{time.UTC, time.FixedZone("local UTC", 0), time.FixedZone("positive", 90*60), time.FixedZone("negative", -5*3600-30*60)} {
		at := time.Date(2026, 10, 8, 12, 3, 4+i, 123456789, zone)
		build, err := CreateFlake(root, "nested/host", root+"/system", &rollback, inputs, info, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Save(build); err != nil {
			t.Fatal(err)
		}
		builds = append(builds, build)
	}
	// An old supported alias/optional-rollback record remains untouched.
	seed(t, root, "nested/host", "old", `{"mode":"flakes","date":"2026-01-01T00:00:00Z","toplevel":"/fake/old","inputs":{},"inputsInfo":{"legacy":"old"},"extra":"retained"}`)
	list, err := LoadBuildList(root, "nested/host", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetCurrent(list, builds[2]); err != nil {
		t.Fatal(err)
	}
	for _, mixed := range []bool{false, true} {
		name := "clean-reader"
		if mixed {
			name = "mixed-reader"
			seed(t, root, "nested/host", "rejected", `{"mode":"swpins"}`)
			seed(t, root, "nested/host", "partial", `{"mode":`)
		}
		readOnly(t, root, func() {
			out, stderr := writerRubyCommand(t, name, writerOriginalReader, root)
			var observed struct {
				Ruby, JSON, Current string
				JSONProvider        string `json:"json_provider"`
				Records             []struct {
					Name, Seconds, Fraction string
					UTC                     bool
					Offset                  int
					Fields                  []string
					InputRoles              []string `json:"input_roles"`
					InfoRoles               []string `json:"info_roles"`
					Kernel                  *string
					Links                   map[string]string
					Large, Huge, Floating   *string
					LargeType               *string `json:"large_type"`
				}
				Numeric   struct{ Name, Error string }
				Rejected  []string
				RootLinks map[string]string `json:"root_links"`
			}
			// Decode only primitive observations; large original numbers are strings.
			if err := json.Unmarshal(out, &observed); err != nil {
				t.Fatal(string(out), err)
			}
			if observed.Ruby != "3.4.9" || observed.JSON != "2.21.2" || observed.Current != builds[2].Name {
				t.Fatal(string(out))
			}
			fresh, err := LoadBuildList(root, "nested/host", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if len(observed.Records) != len(fresh.Builds) {
				t.Fatal("reader count mismatch", string(out))
			}
			for i, record := range observed.Records {
				build := fresh.Builds[i]
				if record.Name != build.Name || record.Seconds != build.Date.Seconds().String() || record.Fraction != "0/1" || record.UTC != build.Date.UTC || record.Offset != build.Date.OffsetSeconds {
					t.Fatal("date/order mismatch", record, build)
				}
				if record.Name == "old" {
					if record.Links["auto_rollback"] != "" || !reflect.DeepEqual(record.InfoRoles, []string{"legacy"}) {
						t.Fatal("old record normalized")
					}
					continue
				}
				if !reflect.DeepEqual(record.Fields, []string{"mode", "date", "toplevel", "auto_rollback", "inputs", "inputs_info"}) || !reflect.DeepEqual(record.InputRoles, []string{"z", "a"}) || !reflect.DeepEqual(record.InfoRoles, []string{"z", "a"}) || record.Kernel == nil || *record.Kernel != "6.12.3" || record.Large == nil || *record.Large != "9007199254740993" || record.LargeType == nil || *record.LargeType != "Integer" || record.Huge == nil || *record.Huge != "999999999999999999999999999999" || record.Floating == nil || *record.Floating != "Float" {
					t.Fatal("metadata value/type/order mismatch", record)
				}
				for path, target := range map[string]string{"toplevel": build.Toplevel, "auto_rollback": *build.AutoRollback, "z.input": "/fake/z", "a.input": "/fake/a"} {
					if record.Links[path] != target {
						t.Fatal("literal link differs", record.Links)
					}
				}
			}
			for _, build := range builds {
				for component, path := range map[string]string{"toplevel": "toplevel", "auto_rollback": "auto_rollback", "input.z": "z.input", "input.a": "a.input"} {
					key := RootName(build.Host, build.Name, component)
					if observed.RootLinks[key] != build.Dir+"/"+path {
						t.Fatal("root target differs", key, observed.RootLinks[key])
					}
					writerLink(t, roots+"/"+key, build.Dir+"/"+path)
				}
			}
			if len(observed.RootLinks) != len(builds)*4 {
				t.Fatal("unexpected roots")
			}
			if mixed {
				if observed.Numeric.Error == "" || len(observed.Rejected) != 2 || len(stderr) == 0 {
					t.Fatal("mixed rejected selection lost", string(out), string(stderr))
				}
			} else if observed.Numeric.Error != "" || observed.Numeric.Name != fresh.Builds[len(fresh.Builds)-1].Name || len(stderr) != 0 {
				t.Fatal("clean numeric/error mismatch", string(out), string(stderr))
			}
		})
	}
}

const writerOriginalFailure = `
gem 'json', '2.21.2'
require 'json'
require 'time'
require 'fileutils'
$LOAD_PATH.unshift(ARGV.fetch(0))
root = ARGV.fetch(1)
module ConfCtl
  module Generation; end
  class Error < StandardError; end
  class ConfDir
    class << self; attr_accessor :generation_dir; end
  end
  module GCRoot
    def self.add(*args); raise 'unexpected root'; end
  end
  def self.safe_host_name(host); host.gsub('/', ':'); end
end
require 'confctl/generation/build'
ConfCtl::ConfDir.generation_dir = File.join(root, '.confctl', 'generations')
original_open = File.method(:open)
mode = ARGV.fetch(2)
File.define_singleton_method(:open) do |path, flags, &block|
  original_open.call(path, flags) do |file|
    file.define_singleton_method(:puts) do |data|
      write(data[0, 7]) if mode.start_with?('write')
      IO.for_fd(fileno, autoclose: true).close if mode.end_with?('close')
      raise 'fixture body error' if mode.start_with?('write')
      super(data)
    end
    if mode.start_with?('encode')
      JSON.define_singleton_method(:pretty_generate) do |*args|
        IO.for_fd(file.fileno, autoclose: true).close if mode.end_with?('close')
        raise 'fixture encode error'
      end
    end
    block.call(file)
  end
end
gen = ConfCtl::Generation::Build.new('host')
gen.create_flake('/fake/top', '/fake/rollback', inputs: {}, inputs_info: {}, date: Time.utc(2026, 10, 8, 12))
error = nil
begin
  gen.save
rescue => e
  error = {class: e.class.name, message: e.message}
end
puts JSON.generate(mode: mode, error: error,
  residue: File.binread(File.join(gen.dir, 'generation.json')),
  entries: Dir.children(gen.dir).sort,
  ruby: RUBY_VERSION, json: JSON::VERSION)
`

func TestWriterOriginalRubyFailurePrecedence(t *testing.T) {
	_ = rubyAdapter(t)
	for _, mode := range []string{"encode", "write", "encode+close", "write+close"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			out, stderr := writerRubyCommand(t, "failure-"+mode, writerOriginalFailure, root, mode)
			var observed struct {
				Mode, Residue, Ruby, JSON string
				Error                     struct{ Class, Message string }
				Entries                   []string
			}
			if err := json.Unmarshal(out, &observed); err != nil {
				t.Fatal(string(out), err)
			}
			if observed.Ruby != "3.4.9" || observed.JSON != "2.21.2" || len(stderr) != 0 || !reflect.DeepEqual(observed.Entries, []string{"auto_rollback", "generation.json", "toplevel"}) {
				t.Fatal(string(out), string(stderr))
			}
			if mode == "write+close" {
				// This descriptor fault precedes Ruby's buffered prefix flush.
				// It establishes close precedence with empty residue, not a
				// prediction of a normal disk-full write's surviving bytes.
				if observed.Residue != "" {
					t.Fatal("forced descriptor-close residue differs", observed)
				}
			} else if strings.HasPrefix(mode, "write") {
				if observed.Residue != "{\n  \"mo" {
					t.Fatal("write residue differs", observed)
				}
			} else if observed.Residue != "" {
				t.Fatal("encoding did not follow truncation", observed)
			}
			if strings.HasSuffix(mode, "close") {
				if observed.Error.Class != "Errno::EBADF" {
					t.Fatal("original close precedence differs", observed)
				}
			} else if observed.Error.Class != "RuntimeError" || observed.Error.Message != "fixture "+strings.Split(mode, "+")[0]+" error" && observed.Error.Message != "fixture body error" {
				t.Fatal(observed)
			}
		})
	}
}

const writerOriginalNesting = `
gem 'json', '2.21.2'
require 'json'
require 'time'
$LOAD_PATH.unshift(ARGV.fetch(0))
root = ARGV.fetch(1)
module ConfCtl
  module Generation; end
  module Utils; end
  class Error < StandardError; end
  class ConfDir
    class << self; attr_accessor :generation_dir; end
  end
  def self.safe_host_name(host); host.gsub('/', ':'); end
end
require 'confctl/generation/build'
require 'confctl/gcroot'
ConfCtl::ConfDir.generation_dir = File.join(root, '.confctl', 'generations')
ConfCtl::GCRoot.define_singleton_method(:dir) { File.join(root, 'private-gcroots') }
arrays = Integer(ARGV.fetch(2))
value = 'leaf'
arrays.times { value = [value] }
gen = ConfCtl::Generation::Build.new('host')
gen.create_flake('/fake/top', '/fake/rollback', inputs: {}, inputs_info: {'nested' => value}, date: Time.utc(2026, 10, 8, 12))
error = nil
begin
  gen.save
rescue => e
  error = {class: e.class.name, message: e.message}
end
puts JSON.generate(ruby: RUBY_VERSION, json: JSON::VERSION, arrays: arrays,
  error: error, bytes: File.size(File.join(gen.dir, 'generation.json')),
  links: Dir.children(gen.dir).sort,
  root_count: Dir.exist?(ConfCtl::GCRoot.dir) ? Dir.children(ConfCtl::GCRoot.dir).length : 0)
`

func TestWriterOriginalRubyNestingBoundary(t *testing.T) {
	_ = rubyAdapter(t)
	for _, arrays := range []int{97, 98, 99} {
		out, stderr := writerRubyCommand(t, fmt.Sprintf("nesting-%d", arrays), writerOriginalNesting, t.TempDir(), fmt.Sprint(arrays))
		var observed struct {
			Ruby, JSON    string
			Arrays, Bytes int
			Error         *struct{ Class, Message string }
			Links         []string
			RootCount     int `json:"root_count"`
		}
		if err := json.Unmarshal(out, &observed); err != nil {
			t.Fatal(string(out), err)
		}
		if observed.Ruby != "3.4.9" || observed.JSON != "2.21.2" || observed.Arrays != arrays || len(stderr) != 0 || !reflect.DeepEqual(observed.Links, []string{"auto_rollback", "generation.json", "toplevel"}) {
			t.Fatal(string(out), string(stderr))
		}
		if arrays <= 98 {
			if observed.Error != nil || observed.Bytes == 0 || observed.RootCount != 2 {
				t.Fatal("original valid depth refused", string(out))
			}
		} else if observed.Error == nil || observed.Error.Class != "JSON::NestingError" || observed.Bytes != 0 || observed.RootCount != 0 {
			t.Fatal("original excessive depth did not fail after truncation", string(out))
		}
	}
}
