# frozen_string_literal: true

require 'json'
require 'tmpdir'

RSpec.describe 'swpins workflows' do
  include CliHelper

  def create_repo_with_commit(path, content, message)
    File.write(File.join(path, 'dummy.txt'), content)
    git_commit_all(path, message)
    run_cmd('git', 'rev-parse', 'HEAD', chdir: path).out.strip
  end

  def channel_rev(conf_dir)
    json = JSON.parse(File.read(File.join(conf_dir, 'swpins', 'channels', 'nixos-unstable.json')))
    json.fetch('nixpkgs').fetch('state').fetch('rev')
  end

  it 'updates and sets swpins from a local git repository with commit variants' do
    Dir.mktmpdir('confctl-swpins-spec-') do |dir|
      nixpkgs_path = ENV.fetch('CONFCTL_TEST_NIXPKGS', nil)
      if nixpkgs_path.nil? || nixpkgs_path.empty?
        lookup = run_cmd('nix-instantiate', '--find-file', 'nixpkgs', chdir: dir)
        expect(lookup.success?).to be(true), "#{lookup.out}\n#{lookup.err}"
        nixpkgs_path = lookup.out.strip
      end

      dummy_repo = File.join(dir, 'dummy-repo')
      FileUtils.mkdir_p(dummy_repo)
      init_git_repo(dummy_repo)
      rev_a = create_repo_with_commit(dummy_repo, "A\n", 'dummy: A')

      branch = run_cmd('git', 'rev-parse', '--abbrev-ref', 'HEAD', chdir: dummy_repo).out.strip

      conf_dir = File.join(dir, 'conf')
      FileUtils.mkdir_p(conf_dir)
      expect(run_confctl('init', '--swpins', chdir: conf_dir).success?).to be(true)

      File.write(File.join(conf_dir, 'configs', 'swpins.nix'), <<~NIX)
        { config, ... }:
        {
          confctl.swpins.core.pins.nixpkgs = {
            type = "directory";
            directory.path = "#{nixpkgs_path}";
          };

          confctl.swpins.channels = {
            nixos-unstable = {
              nixpkgs = {
                type = "git-rev";
                git-rev = {
                  url = "file://#{dummy_repo}";
                  fetchSubmodules = false;
                  update = {
                    auto = false;
                    interval = 0;
                    ref = "refs/heads/#{branch}";
                  };
                };
              };
            };
          };
        }
      NIX

      init_git_repo(conf_dir)
      git_commit_all(conf_dir, 'fixture: initial')

      res =
        if ENV['CONFCTL_RSPEC_SANDBOX'] == '1'
          run_confctl('swpins', 'channel', 'update', 'nixos-unstable', 'nixpkgs', chdir: conf_dir)
        else
          run_confctl('swpins', 'update', chdir: conf_dir)
        end
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(channel_rev(conf_dir)).to eq(rev_a)

      rev_b = create_repo_with_commit(dummy_repo, "B\n", 'dummy: B')

      res = run_confctl(
        'swpins', 'channel', 'update',
        '--commit', '--no-changelog', '--no-editor',
        'nixos-unstable', 'nixpkgs',
        chdir: conf_dir
      )
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(channel_rev(conf_dir)).to eq(rev_b)

      log_count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: conf_dir).out.strip.to_i
      expect(log_count).to eq(2)

      res = run_confctl(
        'swpins', 'channel', 'set',
        '--commit', '--changelog', '--no-editor',
        'nixos-unstable', 'nixpkgs', rev_a,
        chdir: conf_dir
      )
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(channel_rev(conf_dir)).to eq(rev_a)

      log_count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: conf_dir).out.strip.to_i
      expect(log_count).to eq(3)
    end
  end
end
