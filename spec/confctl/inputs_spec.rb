# frozen_string_literal: true

require 'json'
require 'tmpdir'

RSpec.describe 'inputs workflows' do
  include CliHelper

  def create_dummy_repo(path)
    FileUtils.mkdir_p(path)
    init_git_repo(path)

    File.write(
      File.join(path, 'flake.nix'),
      <<~NIX
        {
          description = "dummy flake input for confctl tests";
          outputs = { self, ... }: {};
        }
      NIX
    )
    File.write(File.join(path, 'dummy.txt'), "A\n")
    git_commit_all(path, 'dummy: A')
    rev_a = run_cmd('git', 'rev-parse', 'HEAD', chdir: path).out.strip

    [rev_a]
  end

  def commit_dummy(path, label)
    count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: path).out.strip.to_i
    File.write(File.join(path, 'dummy.txt'), "#{label} #{count + 1}\n")
    git_commit_all(path, "dummy: #{label}")
    run_cmd('git', 'rev-parse', 'HEAD', chdir: path).out.strip
  end

  def locked_rev(lock_path, input)
    lock = JSON.parse(File.read(lock_path))
    node_id = lock.fetch('nodes').fetch('root').fetch('inputs').fetch(input)
    lock.fetch('nodes').fetch(node_id).fetch('locked').fetch('rev')
  end

  it 'updates and sets local git inputs with and without commits/changelog' do
    Dir.mktmpdir('confctl-inputs-spec-') do |dir|
      dummy_repo = File.join(dir, 'dummy-input')
      rev_a, = create_dummy_repo(dummy_repo)

      conf_dir = File.join(dir, 'conf')
      FileUtils.mkdir_p(conf_dir)
      init_git_repo(conf_dir)

      File.write(File.join(conf_dir, 'flake.nix'), <<~NIX)
        {
          description = "confctl inputs test";

          inputs = {
            dummy-input.url = "git+file://#{dummy_repo}";
          };

          outputs = { self, ... }: {};
        }
      NIX

      git_commit_all(conf_dir, 'fixture: initial')

      res = run_confctl('inputs', 'update', 'dummy-input', chdir: conf_dir)
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"

      lock_path = File.join(conf_dir, 'flake.lock')
      expect(File).to exist(lock_path)
      expect(locked_rev(lock_path, 'dummy-input')).to eq(rev_a)

      log_count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: conf_dir).out.strip.to_i
      expect(log_count).to eq(1)

      rev_b = commit_dummy(dummy_repo, 'B')

      res = run_confctl(
        'inputs', 'update', '--commit', '--no-changelog', '--no-editor', 'dummy-input',
        chdir: conf_dir
      )
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(locked_rev(lock_path, 'dummy-input')).to eq(rev_b)

      log_count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: conf_dir).out.strip.to_i
      expect(log_count).to eq(2)

      last_msg = run_cmd('git', 'log', '-1', '--pretty=%s', chdir: conf_dir).out.strip
      expect(last_msg).to include('inputs: update dummy-input')

      res = run_confctl(
        'inputs', 'set', '--commit', '--changelog', '--no-editor', 'dummy-input', rev_a,
        chdir: conf_dir
      )
      expect(res.success?).to be(true), "#{res.out}\n#{res.err}"
      expect(locked_rev(lock_path, 'dummy-input')).to eq(rev_a)

      log_count = run_cmd('git', 'rev-list', '--count', 'HEAD', chdir: conf_dir).out.strip.to_i
      expect(log_count).to eq(3)

      last_msg = run_cmd('git', 'log', '-1', '--pretty=%s', chdir: conf_dir).out.strip
      expect(last_msg).to include('inputs: set dummy-input')
    end
  end
end
