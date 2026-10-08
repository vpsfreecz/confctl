# frozen_string_literal: true

require 'tmpdir'

RSpec.describe 'configuration commands' do
  include CliHelper

  it 'initializes a flake configuration directory' do
    Dir.mktmpdir('confctl-config-init-') do |dir|
      result = run_confctl('init', chdir: dir)
      flake = File.read(File.join(dir, 'flake.nix'))

      expect(result.success?).to be(true), result.err
      expect(File).to exist(File.join(dir, 'flake.nix'))
      expect(File).to exist(File.join(dir, 'cluster', 'cluster.nix'))
      expect(File).to exist(File.join(dir, 'configs', 'confctl.nix'))
      expect(File).to exist(File.join(dir, 'data', 'ssh-keys.nix'))
      expect(flake).to include('nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";')
      expect(flake).to include('# vpsadminos.url = "github:vpsfreecz/vpsadminos/staging";')
      expect(flake).to include('# vpsadminos.inputs.nixpkgs.follows = "nixpkgs";')
      expect(flake).to include('confctl.lib.mkConfigDevShell')
      expect(flake).to include('mode = "minimal";')
    end
  end

  it 'adds and renames machines and rewrites cluster inventory' do
    Dir.mktmpdir('confctl-config-add-rename-') do |dir|
      expect(run_confctl('init', chdir: dir).success?).to be(true)

      add_result = run_confctl('add', 'nested/test-machine', chdir: dir)
      expect(add_result.success?).to be(true), add_result.err
      expect(File).to exist(File.join(dir, 'cluster', 'nested', 'test-machine', 'module.nix'))
      expect(File.read(File.join(dir, 'cluster', 'cluster.nix'))).to include('./nested/test-machine/module.nix')

      rename_result = run_confctl('rename', 'nested/test-machine', 'renamed/machine', chdir: dir)
      expect(rename_result.success?).to be(true), rename_result.err
      expect(File).not_to exist(File.join(dir, 'cluster', 'nested', 'test-machine'))
      expect(File).to exist(File.join(dir, 'cluster', 'renamed', 'machine', 'module.nix'))

      cluster_nix = File.read(File.join(dir, 'cluster', 'cluster.nix'))
      expect(cluster_nix).to include('./renamed/machine/module.nix')
      expect(cluster_nix).not_to include('./nested/test-machine/module.nix')
    end
  end
end

RSpec.describe 'flake-only configuration validation' do
  include CliHelper

  it 'rejects removed commands and init flags without changing the directory' do
    Dir.mktmpdir do |dir|
      [%w[swpins], %w[migrate swpins-to-flakes], %w[init --swpins], %w[init --legacy]].each do |command|
        result = run_confctl(*command, chdir: dir)
        expect(result.success?).to be(false)
        expect(Dir.children(dir)).to be_empty
      end
      result = run_confctl('help', chdir: dir)
      expect(result.success?).to be(true)
      expect(result.out).not_to match(/swpins|migrate/)
    end
  end

  it 'requires a flake before adding, renaming or rediscovering machines' do
    Dir.mktmpdir do |dir|
      FileUtils.mkdir_p(File.join(dir, 'cluster', 'old'))
      File.write(File.join(dir, 'cluster', 'cluster.nix'), 'unchanged')
      [%w[add new], %w[rename old new], %w[rediscover]].each do |command|
        result = run_confctl(*command, chdir: dir)
        expect(result.success?).to be(false)
        expect(result.err).to include('has no flake.nix', 'confctl v3')
        expect(File.read(File.join(dir, 'cluster', 'cluster.nix'))).to eq('unchanged')
        expect(File).not_to exist(File.join(dir, 'cluster', 'new'))
        expect(File).to exist(File.join(dir, 'cluster', 'old'))
      end
    end
  end

  it 'uses the generated channel and correct nested import depth' do
    Dir.mktmpdir do |dir|
      expect(run_confctl('init', chdir: dir).success?).to be(true)
      expect(run_confctl('add', 'nested/host', chdir: dir).success?).to be(true)
      expect(File.read(File.join(dir, 'cluster/nested/host/module.nix')))
        .to include('inputs.channels = [ "nixos-unstable" ];')
      expect(File.read(File.join(dir, 'cluster/nested/host/config.nix')))
        .to include('../../../environments/base.nix')
      expect(File.read(File.join(dir, 'configs/confctl.nix'))).to include('# list.columns = [')
      expect(File).not_to exist(File.join(dir, 'swpins'))
    end
  end
end
