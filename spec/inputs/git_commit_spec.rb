# frozen_string_literal: true

require 'spec_helper'
require 'confctl/inputs/git_commit'
require 'tmpdir'

RSpec.describe ConfCtl::Inputs::GitCommit do
  include CliHelper

  let(:message) do
    changelog = "12345678 Preserve complete changelog text\n" * 30_000
    "inputs: update fixture\n\n#{changelog}"
  end

  around do |example|
    Dir.mktmpdir('confctl-git-commit-') do |dir|
      @conf_dir = dir
      @message_paths = []
      init_git_repo(dir)
      run_cmd('git', 'config', 'core.hooksPath', '.git/hooks', chdir: dir)
      File.write(File.join(dir, 'flake.lock'), "baseline\n")
      File.write(File.join(dir, 'unrelated'), "baseline\n")
      git_commit_all(dir, 'fixture: baseline')
      @original_head = git('rev-parse', 'HEAD').out
      File.write(File.join(dir, 'flake.lock'), "updated\n")
      File.write(File.join(dir, 'unrelated'), "keep staged\n")
      git('add', 'unrelated')
      example.run
    end
  end

  before do
    allow(Kernel).to receive(:system).and_wrap_original do |original, *args|
      path = capture_message(args)
      result = original.call(*args)
      expect(File).to exist(path)
      result
    end
  end

  def git(*args)
    run_cmd('git', *args, chdir: @conf_dir).tap do |result|
      raise "git #{args.first} failed: #{result.err}" unless result.success?
    end
  end

  def capture_message(args)
    path = args.fetch(args.index('-F') + 1)
    expect(path).to eq(File.absolute_path(path))
    expect(File.binread(path)).to eq(message)
    expect(File.stat(path).mode & 0o077).to eq(0)
    expect(Dir.pwd).to eq(@conf_dir)
    @message_paths << path
    File.write(File.join(@conf_dir, '.message-path'), path)
    path
  end

  def install_hook(status: 0)
    path = File.join(@conf_dir, '.git/hooks/pre-commit')
    File.write(path, <<~SH)
      #!/bin/sh
      set -eu
      cat "$(cat .message-path)" > hook-message
      printf 'ran\n' > hook-ran
      exit #{status}
    SH
    File.chmod(0o755, path)
  end

  def commit(editor: false, files: ['flake.lock'])
    described_class.commit!(conf_dir: @conf_dir, message: message, editor: editor, files: files)
  end

  def expect_message_removed
    expect(@message_paths).not_to be_empty
    @message_paths.each { |path| expect(File).not_to exist(path) }
  end

  it 'commits the full oversized message and only selected paths through a real hook' do
    expect(message.bytesize).to be >= 1_200_000
    install_hook
    original_dir = Dir.pwd

    commit

    expect(Dir.pwd).to eq(original_dir)
    expect(git('cat-file', 'commit', 'HEAD').out.split("\n\n", 2).last).to eq(message)
    expect(git('diff-tree', '--no-commit-id', '--name-only', '-r', 'HEAD').out).to eq("flake.lock\n")
    expect(git('diff', '--cached', '--name-only').out).to eq("unrelated\n")
    expect(File.read(File.join(@conf_dir, 'hook-ran'))).to eq("ran\n")
    expect(File.binread(File.join(@conf_dir, 'hook-message'))).to eq(message)
    expect_message_removed
  end

  it 'propagates real hook rejection without changing HEAD and removes the message' do
    install_hook(status: 1)

    expect { commit }.to raise_error(RuntimeError, 'git commit failed')

    expect(git('rev-parse', 'HEAD').out).to eq(@original_head)
    expect(File.binread(File.join(@conf_dir, 'hook-message'))).to eq(message)
    expect_message_removed
  end

  [[false, false], [false, true], [true, false], [true, true]].each do |editor, tty|
    it "preserves file arguments with editor=#{editor} and tty=#{tty}" do
      allow($stdout).to receive(:tty?).and_return(tty)
      expect(Kernel).to receive(:system) do |*args|
        path = capture_message(args)
        expected = %w[git commit]
        expected << '-e' if editor && tty
        expect(args).to eq(expected + ['-F', path, 'flake.lock', 'file with spaces'])
        true
      end

      commit(editor: editor, files: ['flake.lock', 'file with spaces'])

      expect_message_removed
    end
  end

  [false, nil].each do |result|
    it "removes the message and preserves the failure when system returns #{result.inspect}" do
      expect(Kernel).to receive(:system) do |*args|
        capture_message(args)
        result
      end

      expect { commit }.to raise_error(RuntimeError, 'git commit failed')

      expect(git('rev-parse', 'HEAD').out).to eq(@original_head)
      expect_message_removed
    end
  end

  it 'removes the message and restores the caller directory when process launch raises' do
    original_dir = Dir.pwd
    expect(Kernel).to receive(:system) do |*args|
      capture_message(args)
      raise Errno::EACCES, 'fixture refusal'
    end

    expect { commit }.to raise_error(Errno::EACCES, /fixture refusal/)

    expect(Dir.pwd).to eq(original_dir)
    expect_message_removed
  end
end
