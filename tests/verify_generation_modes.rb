# frozen_string_literal: true

require 'json'
require 'tmpdir'
require 'fileutils'

def assert!(condition, message)
  raise "Assertion failed: #{message}" unless condition
end

def override_gcroot_dir(base_dir)
  ConfCtl::GCRoot.singleton_class.send(:define_method, :dir) do
    File.join(base_dir, 'gcroots')
  end
end

Dir.mktmpdir('confctl-generation-modes-') do |tmp|
  Dir.chdir(tmp) do
    require 'confctl'

    override_gcroot_dir(ConfCtl::ConfDir.cache_dir)

    host = 'example.test'
    host_dir = File.join(ConfCtl::ConfDir.generation_dir, ConfCtl.safe_host_name(host))
    FileUtils.mkdir_p(host_dir)

    legacy_name = '2026-02-24--00-00-00'
    legacy_dir = File.join(host_dir, legacy_name)
    FileUtils.mkdir_p(legacy_dir)

    legacy_data = {
      'date' => '2026-02-24T12:34:56Z',
      'toplevel' => '/nix/store/legacy-system',
      'auto_rollback' => '/nix/store/legacy-auto-rollback.rb',
      'swpins' => {
        'nixpkgs' => {
          'path' => '/nix/store/legacy-nixpkgs',
          'spec' => {
            'type' => 'git-rev',
            'name' => 'nixpkgs',
            'nix_options' => {
              'url' => 'https://example.com/nixpkgs',
              'fetchSubmodules' => false,
              'update' => {
                'auto' => false,
                'interval' => 0,
                'ref' => nil
              }
            },
            'state' => {
              'rev' => 'deadbeef'
            }
          }
        }
      }
    }

    File.write(File.join(legacy_dir, 'generation.json'), JSON.pretty_generate(legacy_data))

    legacy_gen = ConfCtl::Generation::Build.new(host)
    legacy_gen.load(legacy_name)
    assert!(legacy_gen.mode == 'swpins', 'legacy generation defaults to swpins mode')

    swpin_spec = ConfCtl::Swpins::Spec.for(:'git-rev').new(
      'nixpkgs',
      {
        'url' => 'https://example.com/nixpkgs',
        'fetchSubmodules' => false,
        'update' => {
          'auto' => false,
          'interval' => 0,
          'ref' => nil
        }
      },
      {
        'type' => 'git-rev',
        'name' => 'nixpkgs',
        'nix_options' => {
          'url' => 'https://example.com/nixpkgs',
          'fetchSubmodules' => false,
          'update' => {
            'auto' => false,
            'interval' => 0,
            'ref' => nil
          }
        },
        'state' => {
          'rev' => 'beadfeed'
        }
      }
    )

    swpins_gen = ConfCtl::Generation::Build.new(host)
    swpins_gen.create(
      '/nix/store/new-system',
      '/nix/store/new-auto-rollback.rb',
      { 'nixpkgs' => '/nix/store/new-nixpkgs' },
      { 'nixpkgs' => swpin_spec },
      date: Time.utc(2026, 2, 24, 13, 0, 0)
    )
    swpins_gen.save

    swpins_list = ConfCtl::Generation::BuildList.new(host)
    swpins_loaded = swpins_list[swpins_gen.name]
    assert!(swpins_loaded.mode == 'swpins', 'swpins generation writes mode=swpins')
    assert!(swpins_loaded.swpin_paths == swpins_gen.swpin_paths, 'swpins paths roundtrip')

    flakes_gen = ConfCtl::Generation::Build.new(host)
    flakes_gen.create_flake(
      '/nix/store/flake-system',
      '/nix/store/flake-auto-rollback.rb',
      inputs: { 'nixpkgs' => '/nix/store/flake-nixpkgs' },
      inputs_info: {
        'nixpkgs' => {
          'input' => 'nixpkgs',
          'url' => 'https://github.com/NixOS/nixpkgs',
          'rev' => 'cafebabe',
          'shortRev' => 'cafebabe',
          'lastModified' => 0
        }
      },
      date: Time.utc(2026, 2, 24, 14, 0, 0)
    )
    flakes_gen.save

    flakes_list = ConfCtl::Generation::BuildList.new(host)
    flakes_loaded = flakes_list[flakes_gen.name]
    assert!(flakes_loaded.mode == 'flakes', 'flakes generation writes mode=flakes')
    assert!(flakes_loaded.inputs == flakes_gen.inputs, 'flake inputs roundtrip')
    assert!(flakes_loaded.swpin_specs.empty?, 'flake generation has no swpin specs')

    flakes_json = JSON.parse(File.read(File.join(flakes_loaded.dir, 'generation.json')))
    assert!(!flakes_json.has_key?('swpins'), 'flakes generation json omits swpins')

    assert!(
      swpins_list.find(swpins_loaded.toplevel, swpins_loaded.swpin_paths),
      'find swpins generation by swpin paths'
    )
    assert!(
      flakes_list.find(flakes_loaded.toplevel, flakes_loaded.inputs, mode: 'flakes'),
      'find flakes generation by inputs'
    )
  end
end

puts 'verify_generation_modes.rb: OK'
