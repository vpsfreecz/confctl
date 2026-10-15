package generation

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This is an optional, newly authored ORIGINAL-ADAPTER test. It loads original
// source with explicit private ConfDir/GCRoot injection. It is not an untouched
// packaged CLI observation and is never a dependency of normal Go operation.
func rubyAdapter(t *testing.T) string {
	t.Helper()
	ruby := os.Getenv("CONFCTL_GENERATION_RUBY")
	if ruby == "" {
		t.Skip("optional original adapter: set CONFCTL_GENERATION_RUBY to pinned Ruby3.4.9")
	}
	out, err := exec.Command(ruby, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(out), "ruby 3.4.9 ") {
		t.Fatalf("adapter requires pinned Ruby3.4.9, got %s", out)
	}
	return ruby
}

const originalWriterAdapter = `
require 'json'
require 'time'
require 'fileutils'
require 'securerandom'
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
ConfCtl::GCRoot.define_singleton_method(:dir) { File.join(root, 'adapter-gcroots') }
top = File.join(root, 'system')
FileUtils.mkdir_p(top)
File.symlink('/nix/store/kernel-linux-6.12.3/bzImage', File.join(top, 'kernel'))
input = File.join(root, 'input')
FileUtils.mkdir_p(input)
generations = [Time.utc(2026, 10, 8, 12), Time.utc(2026, 10, 8, 12, 1)].map do |date|
  gen = ConfCtl::Generation::Build.new('nested/host')
  gen.create_flake(top, File.join(root, 'rollback'), inputs: {'nixpkgs' => input},
    inputs_info: {'z' => {'rev' => 'abcdef012345', 'large' => 9007199254740993,
      'nested' => [{'a' => 999999999999999999999999999999, 'b' => nil}]},
      'a' => 'legacy-revision'}, date: date)
  gen.save
  gen
end
list = ConfCtl::Generation::BuildList.new('nested/host')
list.current = generations.first
# A setter supplied a newly created object can append that object to its
# in-memory list. Reload the disk reader, which is the R1 comparison boundary.
list = ConfCtl::Generation::BuildList.new('nested/host')
puts JSON.generate(names: list.to_a.map(&:name), current: list.current.name,
  kernel: list.current.kernel_version, roles: list.current.inputs_info.keys,
  root_links: Dir.children(ConfCtl::GCRoot.dir).sort.to_h { |name|
    [name, File.readlink(File.join(ConfCtl::GCRoot.dir, name))]
  })
`

func TestOriginalRubyWriterToGoReader(t *testing.T) {
	ruby := rubyAdapter(t)
	root := t.TempDir()
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ruby, "-e", originalWriterAdapter, lib, root)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("original private writer: %v\n%s", err, stderr.Bytes())
	}
	var original struct {
		Names           []string
		Current, Kernel string
		Roles           []string
	}
	if err := json.Unmarshal(out, &original); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	readOnly(t, root, func() {
		list, err := LoadBuildList(root, "nested/host", Options{Location: time.UTC})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(names(list), ",") != strings.Join(original.Names, ",") || list.Current.Name != original.Current || *list.Current.KernelVersion != original.Kernel {
			t.Fatalf("Go list %+v; original %+v", list, original)
		}
		if list.Current.InputsInfo[0].Name != original.Roles[0] || list.Current.InputsInfo[1].Name != original.Roles[1] {
			t.Fatal(list.Current.InputsInfo)
		}
		z := list.Current.InputsInfo.value("z").(Object)
		if z.value("large").(json.Number).String() != "9007199254740993" {
			t.Fatal(z)
		}
		nested := z.value("nested").([]any)[0].(Object)
		if nested.value("a").(json.Number).String() != "999999999999999999999999999999" {
			t.Fatal(nested)
		}
		last, err := ResolveBuild(list, "0")
		if err != nil || last.Name != original.Names[1] {
			t.Fatal(last, err)
		}
	})
}

const originalTimeAdapter = `
require 'json'
require 'time'
zone = ARGV.shift
ENV['TZ'] = zone
puts JSON.generate(ARGV.map do |input|
  begin
    t = Time.iso8601(input)
    {input: input, seconds: t.to_i.to_s, fraction: t.subsec.to_s,
      offset: t.utc_offset, utc: t.utc?}
  rescue => e
    {input: input, error: e.message}
  end
end)
`

func TestOriginalRubyTimeAdapter(t *testing.T) {
	ruby := rubyAdapter(t)
	inputs := []string{
		"2026-02-31T12:00:00Z", "2026-02-00T12:00:00Z", "2026-12-31T24:00:00Z",
		"2026-12-31T24:01:00Z", "2026-12-31T23:59:60Z", "2026-10-08T12:00:00+24:00",
		"2026-10-08T12:00:00+01:99", "2026-10-08T12:99:00+01:30", "-1-01-01T00:00:00Z",
		"2026-10-08T12:00:00.123456789123Z", "2026-01-01T24:00:00.1Z",
		"2026-03-29T02:30:00", "2026-10-25T02:30:00", "2026-03-08T02:30:00", "2026-11-01T01:30:00",
		"2026-10-25T01:30:00", "2026-04-05T01:45:00", "2026-10-04T02:15:00", "2011-12-30T12:00:00",
		"0-01-01T00:00:00", "10000-07-01T02:30:00",
		"100000000000000000000-01-01T02:30:00", "100000000000000000000-07-01T02:30:00",
		"-100000000000000000000-01-01T02:30:00", " \t2026-10-08t12:00:00.1z\n",
		"2026-10-08T12:00:00+01", "2026-10-08T12:00:00-0000",
	}
	for _, zone := range []string{"UTC", "Europe/Amsterdam", "America/New_York", "Europe/Dublin", "Australia/Lord_Howe", "Pacific/Apia"} {
		t.Run(zone, func(t *testing.T) {
			location := time.UTC
			if zone != "UTC" {
				// Share the Ruby system TZ database explicitly, including historical offsets.
				tzdir := os.Getenv("TZDIR")
				if tzdir == "" {
					t.Fatal("original local-zone adapter requires explicit TZDIR")
				}
				data, err := os.ReadFile(filepath.Join(tzdir, zone))
				if err != nil {
					t.Fatal(err)
				}
				location, err = time.LoadLocationFromTZData(zone, data)
				if err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"-e", originalTimeAdapter, zone}, inputs...)
			out, err := exec.Command(ruby, args...).Output()
			if err != nil {
				t.Fatal(err)
			}
			var records []struct {
				Input, Seconds, Fraction, Error string
				Offset                          int
				UTC                             bool
			}
			if err := json.Unmarshal(out, &records); err != nil {
				t.Fatal(err)
			}
			for _, r := range records {
				got, err := ParseISO8601(r.Input, location)
				if r.Error != "" {
					if err == nil {
						t.Fatalf("accepted original rejection: %s (%s)", r.Input, r.Error)
					}
					continue
				}
				if err != nil {
					t.Fatal(r.Input, err)
				}
				if got.Seconds().String() != r.Seconds || got.Fraction().RatString() != r.Fraction || got.OffsetSeconds != r.Offset || got.UTC != r.UTC {
					t.Fatalf("%s: Go %s %s %d %v; original %+v", r.Input, got.Seconds(), got.Fraction(), got.OffsetSeconds, got.UTC, r)
				}
			}
		})
	}
}

const originalReviewedAdmissionAdapter = `
require 'json'
require 'time'
abort "expected json2.21.2" unless JSON::VERSION == '2.21.2'
$LOAD_PATH.unshift(ARGV.fetch(0))
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
require 'confctl/generation/build_list'
puts JSON.generate(JSON.parse(STDIN.read).map do |fixture|
  ConfCtl::ConfDir.generation_dir = File.join(fixture['root'], '.confctl', 'generations')
  list = ConfCtl::Generation::BuildList.new('host')
  numeric_error = false
  begin
    list.at_offset(0)
  rescue ConfCtl::Error
    numeric_error = true
  end
  {name: fixture['name'], names: list.to_a.map(&:name),
    rejected: list.rejected.keys, current: list.current && list.current.name,
    numeric_error: numeric_error}
end)
`

func TestOriginalRubyReviewedAdmission(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	jsonLib, err := filepath.Abs("../../.gems/ruby/3.4.0/gems/json-2.21.2/lib")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixtures := []map[string]string{}
	for _, tc := range jsonAdmissionCases() {
		fixture := root + "/" + tc.name
		seed(t, fixture, "host", "good", record(root+"/missing-system", "2026-10-08T12:00:00Z"))
		seed(t, fixture, "host", "candidate", admissionRecord(root+"/missing-system", tc.value))
		fixtures = append(fixtures, map[string]string{"name": tc.name, "root": fixture})
	}
	input, err := json.Marshal(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, root, func() {
		cmd := exec.Command(ruby, "-I", jsonLib, "-e", originalReviewedAdmissionAdapter, lib)
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("original admissions: %v\n%s", err, stderr.Bytes())
		}
		var original []struct {
			Name            string
			Names, Rejected []string
			Current         string
			NumericError    bool `json:"numeric_error"`
		}
		if err := json.Unmarshal(out, &original); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		if len(original) != len(fixtures) {
			t.Fatal("missing original admissions")
		}
		for i, r := range original {
			list, err := LoadBuildList(fixtures[i]["root"], "host", Options{})
			if err != nil {
				t.Fatal(err)
			}
			rejected := []string{}
			for _, v := range list.Rejected {
				rejected = append(rejected, v.Name)
			}
			_, numericErr := ResolveBuild(list, "0")
			if r.Name != fixtures[i]["name"] || strings.Join(names(list), ",") != strings.Join(r.Names, ",") || strings.Join(rejected, ",") != strings.Join(r.Rejected, ",") || list.Current.Name != r.Current || (numericErr != nil) != r.NumericError {
				t.Fatalf("%s: Go names%v rejected%v current%s numeric%v; Ruby %+v", r.Name, names(list), rejected, list.Current.Name, numericErr, r)
			}
		}
	})
}

const originalReviewedPathsAdapter = `
require 'json'
require 'time'
require 'digest'
$LOAD_PATH.unshift(ARGV.fetch(0))
configuration = File.realpath(ARGV.fetch(1))
Dir.chdir(configuration)
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
require 'confctl/generation/build_list'
ConfCtl::ConfDir.generation_dir = File.join(configuration, '.confctl', 'generations')
list = ConfCtl::Generation::BuildList.new('host')
puts JSON.generate(configuration: configuration, hash: Digest::SHA256.hexdigest(configuration),
  names: list.to_a.map(&:name), kernel: list.current.kernel_version)
`

func TestOriginalRubyReviewedPaths(t *testing.T) {
	ruby := rubyAdapter(t)
	lib, err := filepath.Abs("../../lib")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	outer := root + "/outer"
	physical := root + "/actual"
	target := root + "/target"
	for _, dir := range []string{outer, physical + "/config", physical + "/system", target + "/child", target + "/system"} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(physical+"/config", outer+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target+"/child", physical+"/toplink"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/hash-linux-physical/bzImage", target+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nix/store/hash-linux-lexical/bzImage", physical+"/system/kernel"); err != nil {
		t.Fatal(err)
	}
	seed(t, outer, "host", "lexical", record(root+"/missing", "2026-10-08T12:00:00Z"))
	seed(t, physical, "host", "physical", record("toplink/../system", "2026-10-08T12:00:00Z"))
	changeTestDirectory(t, root)
	login := "fixture-user"
	for _, alias := range []string{outer + "/link/..", "outer/link/.."} {
		readOnly(t, root, func() {
			cmd := exec.Command(ruby, "-e", originalReviewedPathsAdapter, lib, alias)
			cmd.Dir = root
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("original paths %v\n%s", err, stderr.Bytes())
			}
			var original struct {
				Configuration, Hash, Kernel string
				Names                       []string
			}
			if err := json.Unmarshal(out, &original); err != nil {
				t.Fatal(err)
			}
			identity, err := ComputeRootIdentity(alias, &login)
			if err != nil {
				t.Fatal(err)
			}
			list, err := LoadBuildList(alias, "host", Options{})
			if err != nil {
				t.Fatal(err)
			}
			if identity.Configuration != original.Configuration || identity.Hash != original.Hash || strings.Join(names(list), ",") != strings.Join(original.Names, ",") || list.Current.KernelVersion == nil || *list.Current.KernelVersion != original.Kernel {
				t.Fatalf("Go %+v %+v; original %+v", identity, list, original)
			}
		})
	}
}
