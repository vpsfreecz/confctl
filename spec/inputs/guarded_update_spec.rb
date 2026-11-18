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
  let(:provider) { instance_double(ConfCtl::Inputs::GitCompare, compare: :ahead) }
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
    allow(ConfCtl::Inputs::GitCompare).to receive(:new).and_return(provider)
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
    expect(provider).to receive(:compare).twice do |**|
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

  it 'skips behind candidates successfully even with --downgrade' do
    allow(provider).to receive(:compare).and_return(:behind)
    response = update(commit: true, downgrade: true)
    expect(response).to include(status: :unchanged, changed: false, changes: [], updated_inputs: [])
    expect(response[:skipped].first).to include(input: 'source', requested_inputs: ['source'])
    expect(response[:skipped].first[:rejections].map(&:kind)).to eq([:behind])
    expect_preserved
  end

  it 'fails on unknown ancestry evidence' do
    allow(provider).to receive(:compare).and_raise(ConfCtl::Inputs::GitCompare::Error, 'unavailable')
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

  it 'skips a whole attempt when it regresses another reachable input' do
    old_data['nodes']['entry']['inputs']['other'] = 'other'
    old_data['nodes']['other'] = { 'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'other') }
    candidate['nodes']['other']['locked']['rev'] = 'c' * 40
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: second input')
    @before_state = repository_state
    allow(provider).to receive(:compare).with(hash_including(repo: 'other')).and_return(:behind)
    expect(update(commit: true)).to include(status: :unchanged, changed: false)
    expect_preserved
  end

  context 'when a selected input has a changed descendant' do
    before do
      old_data['nodes']['source']['inputs'] = { 'nixpkgs' => 'pkgs' }
      old_data['nodes']['pkgs'] = {
        'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'nixpkgs')
      }
      candidate['nodes']['pkgs']['locked']['rev'] = 'c' * 40
      File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
      git_commit_all(@conf_dir, 'fixture: descendant')
      @before_state = repository_state
    end

    it 'retains all result changes while limiting the message to the selected input' do
      mirror = instance_double(ConfCtl::GitRepoMirror)
      expect(ConfCtl::GitRepoMirror).to receive(:new).once.with('https://github.com/example/source', quiet: true).and_return(mirror)
      expect(mirror).to receive(:synchronize).and_yield
      expect(mirror).to receive(:setup)
      expect(mirror).to receive(:log).with(old_rev, new_rev, opts: ['--oneline']).and_return('selected OS log marker')
      expect(ConfCtl::Inputs::GitCommit).to receive(:commit!) do |**args|
        expect(args[:message]).to eq("inputs: update source to bbbbbbbb\n\nsource: aaaaaaaa -> bbbbbbbb\nselected OS log marker")
      end

      response = update(commit: true, changelog: true)

      expect(response).to include(status: :updated, changed: true)
      expect(response[:changes].map(&:name)).to eq(['source', 'source/nixpkgs'])
    end

    it 'commits a descendant-only update with only the requested-input title and no log query' do
      candidate['nodes']['source']['locked']['rev'] = old_rev
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      expect(ConfCtl::Inputs::GitCommit).to receive(:commit!) do |**args|
        expect(args[:message]).to eq('inputs: update source')
      end

      response = update(commit: true, changelog: true)

      expect(response).to include(status: :updated, changed: true)
      expect(response[:changes].map(&:name)).to eq(['source/nixpkgs'])
    end

    it 'does not query logs when Nixpkgs is explicitly selected with no-changelog' do
      candidate['nodes']['source']['locked']['rev'] = old_rev
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      response = update(inputs: ['source/nixpkgs'], commit: true, changelog: false)
      expect(response).to include(status: :updated, changed: true)
      expect(ConfCtl::Inputs::GitCommit).to have_received(:commit!).with(hash_including(message: "inputs: update source/nixpkgs to cccccccc\n\nsource/nixpkgs: aaaaaaaa -> cccccccc"))
    end

    it 'skips a parent attempt with an excluded descendant rewind without installing or committing' do
      allow(provider).to receive(:compare).with(hash_including(repo: 'nixpkgs')).and_return(:behind)
      expect(ConfCtl::GitRepoMirror).not_to receive(:new)
      expect(update(commit: true, changelog: true)).to include(status: :unchanged, changed: false)
      expect(provider).to have_received(:compare).with(hash_including(repo: 'nixpkgs'))
      expect_preserved
    end
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
    mirror = instance_double(ConfCtl::GitRepoMirror)
    expect(ConfCtl::GitRepoMirror).to receive(:new).once.with('https://github.com/example/source', quiet: true).and_return(mirror)
    expect(mirror).to receive(:synchronize).and_yield
    expect(mirror).to receive(:setup)
    expect(mirror).to receive(:log).with(old_rev, new_rev, opts: ['--oneline']).and_return('alias log marker')
    expect(ConfCtl::Inputs::GitCommit).to receive(:commit!) do |**args|
      expect(args[:message]).to eq("inputs: update alias to bbbbbbbb\n\nalias: aaaaaaaa -> bbbbbbbb\nalias log marker")
    end
    response = update(inputs: ['alias'], commit: true, changelog: true)
    expect(response).to include(status: :updated)
    expect(response[:changes].map(&:name)).to eq(['alias', 'parent/leaf'])
    expect(JSON.parse(File.read(File.join(@conf_dir, 'flake.lock'))).dig('nodes', 'parent', 'locked', 'rev')).to eq(old_rev)
  end

  context 'with multiple selected inputs' do
    before do
      %w[other third].each do |name|
        old_data['nodes']['entry']['inputs'][name] = name
        old_data['nodes'][name] = { 'locked' => old_data['nodes']['source']['locked'].merge('repo' => name) }
      end
      File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
      git_commit_all(@conf_dir, 'fixture: independent inputs')
      @before_state = repository_state
      @attempts = []
      allow(cmd).to receive(:run) do |*argv|
        target = argv.last
        reference = JSON.parse(File.read(argv[argv.index('--reference-lock-file') + 1]))
        @attempts << [target, Marshal.load(Marshal.dump(reference))]
        expect(repository_state).to eq(@before_state)
        reference['nodes'][target]['locked']['rev'] = new_rev
        File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(reference))
        result
      end
    end

    it 'uses deterministic attempts, retains accepted references and commits only forward inputs once' do
      allow(provider).to receive(:compare).with(hash_including(repo: 'source')).and_return(:behind)
      expect(ConfCtl::Inputs::GitCommit).to receive(:commit!).once do |**args|
        expect(args[:message]).to eq("inputs: update other, third\n\nother: aaaaaaaa -> bbbbbbbb\nthird: aaaaaaaa -> bbbbbbbb")
      end
      allow(File).to receive(:rename).and_call_original
      expect(File).to receive(:rename).with(anything, File.join(@conf_dir, 'flake.lock')).once.and_call_original
      response = update(inputs: %w[third source other], commit: true)
      expect(response).to include(status: :updated, updated_inputs: %w[other third])
      expect(response[:changes].map(&:name)).to eq(%w[other third])
      expect(response[:skipped].map { |a| a[:input] }).to eq(['source'])
      expect(@attempts.map(&:first)).to eq(%w[other source third])
      expect(@attempts[1][1]['nodes']['other']['locked']['rev']).to eq(new_rev)
      expect(@attempts[2][1]['nodes']['source']['locked']['rev']).to eq(old_rev)
      expect(@attempts[2][1]['nodes']['other']['locked']['rev']).to eq(new_rev)
      final = JSON.parse(File.read(File.join(@conf_dir, 'flake.lock')))
      expect(final['nodes']['source']['locked']['rev']).to eq(old_rev)
      expect(final['nodes']['third']['locked']['rev']).to eq(new_rev)
    end

    %i[diverged error nix metadata source removal].each do |failure|
      it "installs nothing when #{failure} follows accepted temporary work" do
        allow(cmd).to receive(:run) do |*argv|
          target = argv.last
          reference = JSON.parse(File.read(argv[argv.index('--reference-lock-file') + 1]))
          expect(repository_state).to eq(@before_state)
          reference['nodes'][target]['locked']['rev'] = new_rev
          if target == 'source'
            case failure
            when :nix then raise ConfCtl::Error, 'Nix failed'
            when :metadata then reference['nodes'][target]['locked'].merge!('rev' => old_rev, 'narHash' => 'changed')
            when :source then reference['nodes'][target]['locked']['repo'] = 'different'
            when :removal then reference['nodes']['entry']['inputs'].delete('third')
            end
          end
          File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(reference))
          result
        end
        if failure == :error
          allow(provider).to receive(:compare).with(hash_including(repo: 'source')).and_raise(ConfCtl::Inputs::GitCompare::Error, 'no evidence')
        elsif failure == :diverged
          allow(provider).to receive(:compare).with(hash_including(repo: 'source')).and_return(:diverged)
        end
        expected = %i[error nix].include?(failure) ? :error : :rejected
        expect(update(inputs: %w[other source], commit: true)).to include(status: expected, changed: false, changes: [])
        expect_preserved
      end
    end
  end

  it 'does not suppress a hard policy violation alongside a behind path' do
    candidate['nodes']['source']['locked']['narHash'] = 'changed'
    old_data['nodes']['entry']['inputs']['other'] = 'other'
    old_data['nodes']['other'] = { 'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'other') }
    candidate['nodes']['other'] = { 'locked' => old_data['nodes']['other']['locked'].merge('rev' => old_rev, 'narHash' => 'changed') }
    candidate['nodes']['entry']['inputs']['other'] = 'other'
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: mixed rejection')
    @before_state = repository_state
    allow(provider).to receive(:compare).and_return(:behind)
    expect(update(commit: true)).to include(status: :rejected, changed: false)
    expect_preserved
  end

  it 'checks the final accepted graph strictly against the original baseline' do
    allow(provider).to receive(:compare).and_return(:ahead, :behind)
    expect(update(commit: true)).to include(status: :rejected, changed: false)
    expect_preserved
  end

  it 'checks concurrent writers even when every attempt is skipped' do
    allow(provider).to receive(:compare) do |**|
      File.write(File.join(@conf_dir, 'flake.nix'), 'external writer')
      :behind
    end
    expect(update(commit: true)).to include(status: :error, changed: false)
    expect(File.read(File.join(@conf_dir, 'flake.nix'))).to eq('external writer')
    expect_preserved
  end

  it 'deduplicates aliases and keeps their requested names in the skip result' do
    old_data['nodes']['entry']['inputs']['alias'] = ['source']
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: alias')
    @before_state = repository_state
    allow(provider).to receive(:compare).and_return(:behind)
    response = update(inputs: %w[source alias source], commit: true)
    expect(cmd).to have_received(:run).once
    expect(response[:skipped].first).to include(input: 'source', requested_inputs: %w[alias source])
    expect_preserved
  end

  it 'rejects redirection of any selected alias in an otherwise accepted candidate' do
    old_data['nodes']['entry']['inputs']['alias'] = ['source']
    old_data['nodes']['entry']['inputs']['other'] = 'source'
    candidate['nodes']['entry']['inputs']['alias'] = ['other']
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: alias selection')
    @before_state = repository_state
    expect(update(inputs: %w[source alias], commit: true)).to include(status: :error, changed: false)
    expect_preserved
  end

  it 'uses real Nix reference/output locks sequentially without changing a no-op checkout' do
    %w[a b].each do |name|
      FileUtils.mkdir_p(File.join(@conf_dir, name))
      File.write(File.join(@conf_dir, name, 'flake.nix'), '{ outputs = { ... }: {}; }')
    end
    File.write(File.join(@conf_dir, 'flake.nix'), <<~NIX)
      {
        inputs.a.url = "path:./a";
        inputs.b.url = "path:./b";
        outputs = { ... }: {};
      }
    NIX
    File.unlink(File.join(@conf_dir, 'flake.lock'))
    git_commit_all(@conf_dir, 'fixture: local flakes')
    initial = run_cmd('nix', '--extra-experimental-features', 'nix-command flakes', 'flake', 'lock', chdir: @conf_dir)
    expect(initial.success?).to be(true), initial.err
    git_commit_all(@conf_dir, 'fixture: local lock')
    @before_state = repository_state
    allow(ConfCtl::SystemCommand).to receive(:new).and_return(TTY::Command.new(printer: :null))
    expect(update(inputs: %w[b a], commit: true)).to include(status: :unchanged, changed: false, skipped: [])
    expect_preserved
  end

  it 'never lets a later selected parent regress a shared dependency accepted earlier' do
    old_data['nodes']['entry']['inputs']['a-shared'] = 'shared'
    old_data['nodes']['shared'] = { 'locked' => old_data['nodes']['source']['locked'].merge('repo' => 'shared') }
    old_data['nodes']['source']['inputs'] = { 'shared' => ['a-shared'] }
    File.write(File.join(@conf_dir, 'flake.lock'), JSON.generate(old_data))
    git_commit_all(@conf_dir, 'fixture: shared follows')
    @before_state = repository_state
    allow(cmd).to receive(:run) do |*argv|
      reference = JSON.parse(File.read(argv[argv.index('--reference-lock-file') + 1]))
      if argv.last == 'a-shared'
        reference['nodes']['shared']['locked']['rev'] = new_rev
      else
        expect(reference['nodes']['shared']['locked']['rev']).to eq(new_rev)
        reference['nodes']['shared']['locked']['rev'] = old_rev
        reference['nodes']['source']['locked']['rev'] = new_rev
      end
      File.write(argv[argv.index('--output-lock-file') + 1], JSON.generate(reference))
      result
    end
    allow(provider).to receive(:compare).with(hash_including(repo: 'shared', old_rev: new_rev, new_rev: old_rev)).and_return(:behind)
    response = update(inputs: %w[source a-shared], commit: true)
    expect(response).to include(status: :updated, updated_inputs: ['a-shared'])
    expect(response[:skipped].map { |a| a[:input] }).to eq(['source'])
    expect(response[:changes].map(&:name)).to contain_exactly('a-shared', 'source/shared')
    final = JSON.parse(File.read(File.join(@conf_dir, 'flake.lock')))
    expect(final['nodes']['source']['locked']['rev']).to eq(old_rev)
    expect(final['nodes']['shared']['locked']['rev']).to eq(new_rev)
  end
end
