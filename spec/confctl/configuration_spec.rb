# frozen_string_literal: true

require 'tmpdir'

RSpec.describe 'configuration commands' do
  include CliHelper

  it 'initializes a flake configuration directory' do
    Dir.mktmpdir('confctl-config-init-') do |dir|
      result = run_confctl('init', chdir: dir)

      expect(result.success?).to be(true), result.err
      expect(File).to exist(File.join(dir, 'flake.nix'))
      expect(File).to exist(File.join(dir, 'cluster', 'cluster.nix'))
      expect(File).to exist(File.join(dir, 'configs', 'confctl.nix'))
      expect(File).to exist(File.join(dir, 'data', 'ssh-keys.nix'))
      expect(File.read(File.join(dir, 'flake.nix'))).to include('confctl.lib.mkConfigDevShell')
      expect(File.read(File.join(dir, 'flake.nix'))).to include('mode = "minimal";')
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
