# frozen_string_literal: true

require 'spec_helper'
require 'json'
require 'rbconfig'
require 'tmpdir'

RSpec.describe 'packaged CLI caller environment' do
  include CliHelper

  before do
    skip 'requires CONFCTL_BIN pointing to the Nix package' unless ENV['CONFCTL_BIN']
  end

  around do |example|
    Dir.mktmpdir('confctl-package-env-') do |dir|
      @dir = dir
      @conf = File.join(dir, 'conf')
      @caller = File.join(dir, 'caller')
      @preload_marker = File.join(dir, 'preloaded')
      @hook_result = File.join(dir, 'hook-result')
      FileUtils.mkdir_p([@conf, File.join(@caller, 'gem', 'lib'), File.join(dir, 'bin')])
      File.symlink(RbConfig.ruby, File.join(dir, 'bin', 'ruby'))
      File.write(File.join(@caller, 'Gemfile'), "gem 'confctl-caller-fixture', path: 'gem'\n")
      File.write(File.join(@caller, 'gem', 'confctl-caller-fixture.gemspec'), <<~RUBY)
        Gem::Specification.new do |spec|
          spec.name = 'confctl-caller-fixture'
          spec.version = '1.0.0'
          spec.summary = 'Caller bundle fixture'
          spec.authors = ['confctl tests']
          spec.files = ['lib/caller_fixture.rb']
        end
      RUBY
      File.write(File.join(@caller, 'gem', 'lib', 'caller_fixture.rb'), "CALLER_FIXTURE = 'caller bundle loaded'\n")
      @preload = File.join(@caller, 'preload.rb')
      File.write(@preload, <<~RUBY)
        require 'bundler/setup'
        require 'caller_fixture'
        File.write(ENV.fetch('CALLER_PRELOAD_MARKER'), CALLER_FIXTURE)
      RUBY
      File.write(File.join(@conf, 'flake.nix'), '{ outputs = { ... }: {}; }')
      lock = {
        'version' => 7, 'root' => 'root', 'nodes' => {
          'root' => { 'inputs' => { 'nixpkgs' => 'source' } },
          'source' => { 'locked' => { 'type' => 'github', 'owner' => 'NixOS', 'repo' => 'nixpkgs', 'rev' => 'a' * 40 } }
        }
      }
      File.write(File.join(@conf, 'flake.lock'), JSON.generate(lock))
      example.run
    end
  end

  def caller_env
    {
      'RUBYOPT' => "-r#{@preload}",
      'BUNDLE_GEMFILE' => File.join(@caller, 'Gemfile'),
      'BUNDLE_PATH' => File.join(@caller, 'gems'),
      'BUNDLE_APP_CONFIG' => File.join(@caller, '.bundle'),
      # RSpec's bundle exec adds a separate RubyGems preload absent from the caller dev shell.
      'BUNDLER_SETUP' => nil,
      'PATH' => "#{File.join(@dir, 'bin')}:#{ENV.fetch('PATH')}",
      'CALLER_PRELOAD_MARKER' => @preload_marker,
      'CALLER_HOOK_RESULT' => @hook_result
    }
  end

  it 'starts with packaged gems without activating the caller bundle or preload' do
    result = run_confctl('inputs', 'ls', '--json', 'nixpkgs', chdir: @conf, env: caller_env)
    expect(result.success?).to be(true), result.err
    expect(JSON.parse(result.out)).to contain_exactly(include('input' => 'nixpkgs', 'rev' => 'a' * 40))
    expect(File).not_to exist(@preload_marker)
  end

  it 'retains caller settings for a real Git hook using the caller Ruby and bundle' do
    init_git_repo(@conf)
    git_commit_all(@conf, 'fixture: baseline')
    File.write(File.join(@conf, 'hook-probe'), 'commit through confctl')
    FileUtils.mkdir_p(File.join(@conf, 'scripts'))
    File.write(File.join(@conf, 'scripts', 'hook-probe.rb'), <<~RUBY)
      require 'confctl/inputs/git_commit'
      raise 'caller preload changed' unless ENV.fetch('RUBYOPT') == #{caller_env.fetch('RUBYOPT').inspect}
      raise 'caller Gemfile changed' unless ENV.fetch('BUNDLE_GEMFILE') == #{caller_env.fetch('BUNDLE_GEMFILE').inspect}
      raise 'caller bundle path changed' unless ENV.fetch('BUNDLE_PATH') == #{caller_env.fetch('BUNDLE_PATH').inspect}
      ConfCtl::Inputs::GitCommit.commit!(
        conf_dir: Dir.pwd, message: 'fixture: hook probe', editor: false, files: ['hook-probe']
      )
    RUBY
    hook = File.join(@conf, '.git', 'hooks', 'pre-commit')
    # Generated hooks need a declared interpreter, not the host's /usr/bin/env.
    env_bin = ENV.fetch('PATH').split(File::PATH_SEPARATOR)
                 .map { |path| File.join(path, 'env') }.find { |path| File.executable?(path) }
    expect(env_bin).not_to be_nil
    # Preserve env's dispatch name when it is a symlink to a multicall executable.
    File.write(hook, "#!#{File.expand_path(env_bin)} ruby\n" + <<~'RUBY')
      require 'caller_fixture'
      File.write(ENV.fetch('CALLER_HOOK_RESULT'), [
        RbConfig.ruby, CALLER_FIXTURE, Gem.loaded_specs.fetch('confctl-caller-fixture').full_gem_path
      ].join("\n"))
    RUBY
    File.chmod(0o755, hook)
    staged = run_cmd('git', 'add', 'hook-probe', chdir: @conf)
    expect(staged.success?).to be(true), staged.err

    result = run_confctl('inputs', 'ls', '--json', 'nixpkgs', chdir: @conf, env: caller_env)
    expect(result.success?).to be(true), "#{result.out}\n#{result.err}"
    expect(File.read(@preload_marker)).to eq('caller bundle loaded')
    expected = [RbConfig.ruby, 'caller bundle loaded', File.join(@caller, 'gem')]
    expect(File.read(@hook_result).split("\n")).to eq(expected)
    expect(run_cmd('git', 'log', '-1', '--format=%s', chdir: @conf).out.strip).to eq('fixture: hook probe')
  end
end
