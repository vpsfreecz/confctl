# frozen_string_literal: true

require 'spec_helper'
require 'confctl/exceptions'
require 'confctl/inputs/updater'
require 'tty-command'
require 'tmpdir'

RSpec.describe ConfCtl::Inputs::GuardedUpdate do
  include CliHelper

  let(:old_rev) { 'a' * 40 }
  let(:new_rev) { 'b' * 40 }
  let(:provider) { instance_double(ConfCtl::Inputs::GitHubCompare, compare: :ahead) }
  let(:cmd) { instance_double(TTY::Command) }
  let(:result) { TTY::Command::Result.new(0, '', '') }
  let(:old_data) do
    {
      'version' => 7, 'root' => 'entry',
      'nodes' => {
        'entry' => { 'inputs' => { 'source' => 'source' } },
        'source' => {
          'locked' => { 'type' => 'github', 'owner' => 'example', 'repo' => 'source', 'rev' => old_rev }
        }
      }
    }
  end
  let(:candidate) do
    Marshal.load(Marshal.dump(old_data)).tap { |data| data['nodes']['source']['locked']['rev'] = new_rev }
  end

  around do |example|
    Dir.mktmpdir('confctl-guarded-update-') do |dir|
      @conf_dir = dir
      init_git_repo(dir)
      File.write(File.join(dir, 'flake.nix'), "{ outputs = { ... }: {}; }\n")
      File.write(File.join(dir, 'flake.lock'), JSON.pretty_generate(old_data))
      git_commit_all(dir, 'fixture: baseline')
      @before_state = repository_state
      example.run
    end
  end

  before do
    allow(ConfCtl::SystemCommand).to receive(:new).and_return(cmd)
    allow(ConfCtl::Inputs::GitHubCompare).to receive(:new).and_return(provider)
    allow(ConfCtl::Inputs::GitCommit).to receive(:commit!)
    allow(cmd).to receive(:run) do |*argv|
      expect(argv).to include('--reference-lock-file', '--output-lock-file')
      reference = argv[argv.index('--reference-lock-file') + 1]
      output = argv[argv.index('--output-lock-file') + 1]
      expect(File.binread(reference)).to eq(@before_state[:lock])
      expect(repository_state).to eq(@before_state)
      File.write(output, JSON.generate(candidate))
      result
    end
  end

  def repository_state
    index = run_cmd('git', 'rev-parse', '--git-path', 'index', chdir: @conf_dir).out.strip
    {
      lock: File.binread(File.join(@conf_dir, 'flake.lock')),
      head: run_cmd('git', 'rev-parse', 'HEAD', chdir: @conf_dir).out,
      index: File.binread(File.expand_path(index, @conf_dir))
    }
  end

  def update(**options)
    ConfCtl::Inputs::Updater.run!(
      conf_dir: @conf_dir, inputs: ['source'], commit: false,
      changelog: false, downgrade: false, editor: false,
      forward_only: true, **options
    )
  end

  def expect_preserved
    expect(repository_state).to eq(@before_state)
    expect(ConfCtl::Inputs::GitCommit).not_to have_received(:commit!)
    expect(Dir.glob(File.join(@conf_dir, '.confctl-update-*'))).to be_empty
  end

  it 'validates the separate candidate before installing and committing' do
    expect(provider).to receive(:compare) do |**|
      expect(repository_state).to eq(@before_state)
      :ahead
    end
    expect(ConfCtl::Inputs::GitCommit).to receive(:commit!) do |**|
      expect(JSON.parse(File.read(File.join(@conf_dir, 'flake.lock')))).to eq(candidate)
    end

    response = update(commit: true)
    expect(response).to include(status: :updated, changed: true)
    expect(response[:changes].map(&:name)).to eq(['source'])
    expect(response[:changes].first.new_rev).to eq(new_rev)
  end

  it 'rejects non-containing candidates even with --downgrade' do
    allow(provider).to receive(:compare).and_return(:behind)
    expect(update(commit: true, downgrade: true)).to include(status: :rejected, changed: false)
    expect_preserved
  end

  it 'fails on unknown ancestry evidence' do
    allow(provider).to receive(:compare).and_raise(ConfCtl::Inputs::GitHubCompare::Error, 'unavailable')
    expect(update(commit: true)).to include(status: :error, changed: false)
    expect_preserved
  end

  it 'preserves the checkout when Nix exits unsuccessfully' do
    allow(cmd).to receive(:run) { TTY::Command.new(printer: :null).run('sh', '-c', 'exit 1') }
    expect(update(commit: true)).to include(status: :error, changed: false)
    expect_preserved
  end

  it 'rejects malformed output' do
    allow(cmd).to receive(:run) do |*argv|
      File.write(argv[argv.index('--output-lock-file') + 1], '{')
      result
    end
    expect(update(commit: true)).to include(status: :error)
    expect_preserved
  end

  it 'does not accept cached GitHub metadata after Nix succeeds' do
    allow(cmd).to receive(:run) do |*argv|
      File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(candidate))
      TTY::Command::Result.new(0, '', "unable to download 'https://api.github.com/repos/x/y': using cached version")
    end
    expect(update(commit: true)).to include(status: :error)
    expect_preserved
  end

  it 'fails if Nix warns that a selector did not match' do
    allow(cmd).to receive(:run).and_return(TTY::Command::Result.new(0, '', "warning: input 'x' does not match any input"))
    expect(update).to include(status: :error)
    expect_preserved
  end

  it 'cleans owned preparation files on interruption before installation' do
    allow(provider).to receive(:compare).and_raise(Interrupt)
    expect { update(commit: true) }.to raise_error(Interrupt)
    expect_preserved
  end

  it 'keeps exact bytes for a no-change result' do
    allow(cmd).to receive(:run) do |*argv|
      File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(old_data))
      result
    end
    expect(update(commit: true)).to include(status: :unchanged, changed: false)
    expect_preserved
  end

  %w[flake.lock flake.nix imported.txt].each do |path|
    it "rejects a concurrent tracked change to #{path} without overwriting it" do
      if path == 'imported.txt'
        File.write(File.join(@conf_dir, path), 'original')
        git_commit_all(@conf_dir, 'fixture: imported declaration data')
        @before_state = repository_state
      end
      allow(provider).to receive(:compare) do |**|
        File.write(File.join(@conf_dir, path), 'external writer')
        :ahead
      end
      expect(update(commit: true)).to include(status: :error)
      expect(File.read(File.join(@conf_dir, path))).to eq('external writer')
      expect(repository_state.slice(:index, :head)).to eq(@before_state.slice(:index, :head))
      expect(ConfCtl::Inputs::GitCommit).not_to have_received(:commit!)
    end
  end

  it 'rejects a concurrent HEAD change' do
    allow(provider).to receive(:compare) do |**|
      run_cmd('git', 'commit', '--allow-empty', '-m', 'external writer', chdir: @conf_dir)
      :ahead
    end
    expect(update(commit: true)).to include(status: :error)
    expect(File.binread(File.join(@conf_dir, 'flake.lock'))).to eq(@before_state[:lock])
    expect(ConfCtl::Inputs::GitCommit).not_to have_received(:commit!)
  end

  it 'rejects a concurrent index change without undoing the other writer' do
    allow(provider).to receive(:compare) do |**|
      File.write(File.join(@conf_dir, 'other'), 'external writer')
      run_cmd('git', 'add', 'other', chdir: @conf_dir)
      :ahead
    end
    expect(update(commit: true)).to include(status: :error)
    expect(repository_state[:lock]).to eq(@before_state[:lock])
    expect(repository_state[:head]).to eq(@before_state[:head])
    expect(run_cmd('git', 'diff', '--cached', '--name-only', chdir: @conf_dir).out).to eq("other\n")
    expect(ConfCtl::Inputs::GitCommit).not_to have_received(:commit!)
  end

  it 'rejects a dirty index before running Nix' do
    File.write(File.join(@conf_dir, 'other'), 'unrelated staged data')
    run_cmd('git', 'add', 'other', chdir: @conf_dir)
    @before_state = repository_state
    expect(update(commit: true)).to include(status: :error)
    expect(cmd).not_to have_received(:run)
    expect_preserved
  end

  it 'requires an existing tracked lock' do
    File.unlink(File.join(@conf_dir, 'flake.lock'))
    expect(update).to include(status: :error)
    expect(cmd).not_to have_received(:run)
    expect(File).not_to exist(File.join(@conf_dir, 'flake.lock'))
  end

  it 'refuses a simultaneous cooperating update without waiting' do
    path = run_cmd('git', 'rev-parse', '--git-path', 'confctl-input-update.lock', chdir: @conf_dir).out.strip
    File.open(File.expand_path(path, @conf_dir), 'w') do |lock|
      lock.flock(File::LOCK_EX)
      expect(update).to include(status: :error)
      expect(cmd).not_to have_received(:run)
    end
    expect_preserved
  end

  it 'leaves a validated lock installed when the commit hook fails' do
    allow(ConfCtl::Inputs::GitCommit).to receive(:commit!).and_raise('git commit failed')
    expect { update(commit: true) }.to raise_error('git commit failed')
    expect(JSON.parse(File.read(File.join(@conf_dir, 'flake.lock')))).to eq(candidate)
    expect(repository_state.slice(:index, :head)).to eq(@before_state.slice(:index, :head))
  end

  it 'rejects the whole candidate when one of several inputs goes backwards' do
    old_data['nodes']['entry']['inputs']['other'] = 'other'
    old_data['nodes']['other'] = { 'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'other') }
    candidate['nodes']['other']['locked']['rev'] = 'c' * 40
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: second input')
    @before_state = repository_state
    allow(provider).to receive(:compare).with(hash_including(repo: 'other')).and_return(:behind)
    expect(update(commit: true)).to include(status: :rejected)
    expect_preserved
  end

  it 'updates the terminal follows target without widening to its parent' do
    old_data['nodes']['entry']['inputs'] = { 'parent' => 'parent', 'alias' => %w[parent leaf] }
    old_data['nodes']['parent'] = {
      'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'parent'),
      'inputs' => { 'leaf' => 'source' }
    }
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: follows')
    @before_state = repository_state
    allow(cmd).to receive(:run) do |*argv|
      expect(argv.last).to eq('parent/leaf')
      File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(candidate))
      result
    end
    response = update(inputs: ['alias'])
    expect(response).to include(status: :updated)
    expect(response[:changes].map(&:name)).to eq(['alias', 'parent/leaf'])
    expect(JSON.parse(File.read(File.join(@conf_dir, 'flake.lock'))).dig('nodes', 'parent', 'locked', 'rev')).to eq(old_rev)
  end
end
