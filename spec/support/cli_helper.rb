# frozen_string_literal: true

require 'fileutils'
require 'open3'

module CliHelper
  Result = Struct.new(:out, :err, :status) do
    def success?
      status.success?
    end
  end

  def repo_root
    File.expand_path('../..', __dir__)
  end

  def confctl_bin
    ENV.fetch('CONFCTL_BIN', File.join(repo_root, 'bin', 'confctl'))
  end

  def run_confctl(*, chdir:, env: {})
    default_env = {
      'CONFCTL_TTY' => '0',
      'NO_COLOR' => '1',
      'PAGER' => ''
    }

    out, err, status = Open3.capture3(default_env.merge(env), confctl_bin, *, chdir:)
    Result.new(out, err, status)
  end

  def run_cmd(*, chdir:, env: {})
    out, err, status = Open3.capture3(env, *, chdir:)
    Result.new(out, err, status)
  end

  def init_git_repo(path)
    run_cmd('git', 'init', chdir: path).tap do |r|
      raise "git init failed: #{r.err}" unless r.success?
    end
    run_cmd('git', 'config', 'user.email', 'confctl-tests@example.invalid', chdir: path).tap do |r|
      raise "git config user.email failed: #{r.err}" unless r.success?
    end
    run_cmd('git', 'config', 'user.name', 'confctl-tests', chdir: path).tap do |r|
      raise "git config user.name failed: #{r.err}" unless r.success?
    end
  end

  def git_commit_all(path, message)
    run_cmd('git', 'add', '.', chdir: path).tap do |r|
      raise "git add failed: #{r.err}" unless r.success?
    end
    run_cmd('git', 'commit', '-m', message, chdir: path).tap do |r|
      raise "git commit failed: #{r.err}" unless r.success?
    end
  end
end
