# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'
require 'tmpdir'

RSpec.describe 'forward-only input CLI' do
  include CliHelper

  let(:options) { { commit: false, changelog: false, downgrade: true, editor: false, 'forward-only': true } }
  let(:response) { { status: :unchanged, changed: false, changes: [], issues: [] } }
  let(:lock) { instance_double(ConfCtl::FlakeLock, input_info: { short_rev: 'aaaaaaaa' }) }

  before do
    allow(ConfCtl::ConfDir).to receive(:path).and_return('/conf')
    allow(ConfCtl::ConfDir).to receive(:require_flake!)
    allow(ConfCtl::Inputs::Updater).to receive(:run!).and_return(response)
    allow(ConfCtl::FlakeLock).to receive(:load).and_return(lock)
  end

  it 'passes the root guard option without making downgrade an override' do
    command = ConfCtl::Cli::Inputs::Root.new({ color: 'never' }, options, ['source'])
    expect { command.update }.to output("No changes.\n").to_stdout
    expect(ConfCtl::Inputs::Updater).to have_received(:run!).with(hash_including(forward_only: true, downgrade: true))
  end

  it 'passes the channel guard option after selecting and deduplicating targets' do
    command = ConfCtl::Cli::Inputs::Channels.new({ color: 'never' }, options, ['{a,b}', 'nixpkgs'])
    allow(command).to receive(:eval_channels).and_return('a' => { 'nixpkgs' => 'source' }, 'b' => { 'nixpkgs' => 'source' })
    expect { command.update }.to output(/Updating nixpkgs in a/).to_stdout
    expect(ConfCtl::Inputs::Updater).to have_received(:run!).with(hash_including(forward_only: true, inputs: ['source']))
  end

  it 'passes the machine guard option' do
    command = ConfCtl::Cli::Inputs::Machines.new({ color: 'never' }, options, %w[host nixpkgs])
    nix = instance_double(ConfCtl::Nix)
    allow(ConfCtl::Nix).to receive(:new).and_return(nix)
    allow(nix).to receive(:eval_inputs_info).with('host').and_return('nixpkgs' => { 'input' => 'source' })
    expect { command.update }.to output(/Updating nixpkgs in host/).to_stdout
    expect(ConfCtl::Inputs::Updater).to have_received(:run!).with(hash_including(forward_only: true, inputs: ['source']))
  end

  it 'propagates a known rejection as exit 3 without printing success' do
    response.merge!(status: :rejected, issues: ['source is behind'])
    command = ConfCtl::Cli::Inputs::Root.new({ color: 'never' }, options, ['source'])
    expect { command.update }.to raise_error(GLI::CustomExit) { |e| expect(e.exit_code).to eq(3) }
  end

  it 'propagates unknown evidence as an error without printing success' do
    response.merge!(status: :error, issues: ['GitHub unavailable'])
    command = ConfCtl::Cli::Inputs::Root.new({ color: 'never' }, options, ['source'])
    expect { command.update }.to raise_error(ConfCtl::Error, /GitHub unavailable/)
  end

  [%w[inputs update], %w[inputs channel update], %w[inputs machine update]].each do |command|
    it "advertises --forward-only for #{command.join(' ')}" do
      Dir.mktmpdir('confctl-guard-cli-') do |dir|
        res = run_confctl(*command, '--help', chdir: dir)
        expect(res.success?).to be(true), res.err
        expect(res.out).to include('--[no-]forward-only')
      end
    end
  end
end

RSpec.describe 'check-update CLI statuses' do
  include CliHelper

  let(:old_lock) do
    {
      'version' => 7, 'root' => 'entry',
      'nodes' => {
        'entry' => { 'inputs' => { 'source' => 'source' } },
        'source' => { 'locked' => { 'type' => 'path', 'path' => '/unchanged/source', 'narHash' => 'sha256-old' } }
      }
    }
  end

  def check_locks(candidate)
    Dir.mktmpdir('confctl-check-update-') do |dir|
      old_path = File.join(dir, 'old.lock')
      new_path = File.join(dir, 'new.lock')
      File.write(old_path, JSON.generate(old_lock))
      File.write(new_path, JSON.generate(candidate))
      old_bytes = File.binread(old_path)
      new_bytes = File.binread(new_path)
      res = run_confctl('inputs', 'check-update', '--old-lock', old_path, '--new-lock', new_path, chdir: dir)
      expect(File.binread(old_path)).to eq(old_bytes)
      expect(File.binread(new_path)).to eq(new_bytes)
      yield res
    end
  end

  it 'accepts an unchanged lock without requiring a confctl configuration' do
    check_locks(old_lock) do |res|
      expect(res.status.exitstatus).to eq(0), res.err
      expect(res.out).to include('No changes.')
    end
  end

  it 'returns 3 when a previously reachable dependency is removed' do
    candidate = Marshal.load(Marshal.dump(old_lock))
    candidate['nodes']['entry']['inputs'] = {}
    check_locks(candidate) do |res|
      expect(res.status.exitstatus).to eq(3), res.err
      expect(res.err).to include('previously reachable dependency was removed')
    end
  end

  it 'returns 1 for a changed unsupported source' do
    candidate = Marshal.load(Marshal.dump(old_lock))
    candidate['nodes']['source']['locked']['narHash'] = 'sha256-new'
    check_locks(candidate) do |res|
      expect(res.status.exitstatus).to eq(1), res.err
      expect(res.err).to include('could not be verified')
    end
  end

  it 'returns 1 for an invalid graph' do
    check_locks({}) do |res|
      expect(res.status.exitstatus).to eq(1), res.err
      expect(res.err).to include('version-7')
    end
  end
end

RSpec.describe 'guarded channel update compatibility' do
  include CliHelper

  it 'works with old-style settings and channels without moduleOptions' do
    Dir.mktmpdir('confctl-legacy-channels-') do |dir|
      init_git_repo(dir)
      FileUtils.mkdir_p(File.join(dir, 'source'))
      File.write(File.join(dir, 'source', 'flake.nix'), '{ outputs = { ... }: {}; }')
      File.write(File.join(dir, 'flake.nix'), <<~NIX)
        {
          inputs.source.url = "path:./source";
          outputs = { ... }: {
            confctl.settings.nix.maxJobs = "auto";
            confctl.channels.legacy.nixpkgs = "source";
          };
        }
      NIX
      git_commit_all(dir, 'fixture: legacy channels')
      initial = run_cmd('nix', '--extra-experimental-features', 'nix-command flakes', 'flake', 'lock', chdir: dir)
      expect(initial.success?).to be(true), initial.err
      git_commit_all(dir, 'fixture: initial lock')
      original = File.binread(File.join(dir, 'flake.lock'))
      res = run_confctl('inputs', 'channel', 'update', '--forward-only', 'legacy', 'nixpkgs', chdir: dir)
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(res.out).to include('Updating nixpkgs in legacy')
      expect(File.binread(File.join(dir, 'flake.lock'))).to eq(original)
    end
  end
end

RSpec.describe 'input listing JSON' do
  include CliHelper

  it 'exposes full follows-resolved revisions and preserves the default table' do
    Dir.mktmpdir('confctl-input-json-') do |dir|
      File.write(File.join(dir, 'flake.nix'), '{ outputs = { ... }: {}; }')
      data = {
        'version' => 7, 'root' => 'entry',
        'nodes' => {
          'entry' => { 'inputs' => { 'parent' => 'parent', 'nixpkgs' => %w[parent leaf], 'path' => 'path' } },
          'parent' => { 'locked' => {}, 'inputs' => { 'leaf' => 'renumbered' } },
          'renumbered' => { 'locked' => { 'type' => 'github', 'owner' => 'NixOS', 'repo' => 'nixpkgs', 'rev' => 'a' * 40 } },
          'path' => { 'locked' => { 'type' => 'path', 'path' => '/example' } }
        }
      }
      File.write(File.join(dir, 'flake.lock'), JSON.generate(data))
      res = run_confctl('inputs', 'ls', '--json', 'nixpkgs', chdir: dir)
      expect(res.success?).to be(true), res.err
      expected = [
        { 'input' => 'nixpkgs', 'type' => 'github', 'ref' => nil, 'rev' => 'a' * 40, 'url' => 'https://github.com/NixOS/nixpkgs' }
      ]
      expect(JSON.parse(res.out)).to eq(expected)
      table = run_confctl('inputs', 'ls', 'nixpkgs', chdir: dir)
      expect(table.success?).to be(true), table.err
      expect(table.out).to include('aaaaaaaa')
      expect(table.out).not_to include('a' * 40)
      absent = run_confctl('inputs', 'ls', '--json', 'path', chdir: dir)
      expect(absent.success?).to be(true), absent.err
      expect(JSON.parse(absent.out).first).to include('input' => 'path', 'rev' => nil)
      unmatched = run_confctl('inputs', 'ls', '--json', 'missing', chdir: dir)
      expect(unmatched.success?).to be(true), unmatched.err
      expect(JSON.parse(unmatched.out)).to eq([])
    end
  end
end
