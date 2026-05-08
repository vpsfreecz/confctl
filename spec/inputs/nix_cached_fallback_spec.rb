# frozen_string_literal: true

require 'spec_helper'
require 'confctl/exceptions'
require 'confctl/inputs/setter'
require 'confctl/inputs/updater'
require 'tmpdir'
require 'tty-command'

RSpec.describe 'Nix cached fallback handling' do
  def cached_fallback_result
    stderr = <<~ERR
      warning: error: unable to download 'https://api.github.com/repos/vpsfreecz/vpsadminos/commits/staging': HTTP error 403
      {"message":"API rate limit exceeded"}; using cached version
    ERR

    TTY::Command::Result.new(0, '', stderr)
  end

  let(:cmd) { instance_double(TTY::Command) }

  before do
    allow(ConfCtl::SystemCommand).to receive(:new).and_return(cmd)
    allow(cmd).to receive(:run).and_return(cached_fallback_result)
  end

  it 'fails flake updates that used cached GitHub metadata' do
    Dir.mktmpdir do |dir|
      expect do
        ConfCtl::Inputs::Updater.send(:run_nix_flake_update!, dir, ['vpsadminos'])
      end.to raise_error(ConfCtl::Error, /GitHub API rate limit was exceeded/)
    end
  end

  it 'fails flake locks that used cached GitHub metadata' do
    Dir.mktmpdir do |dir|
      tmpfile = File.join(dir, 'flake.lock.tmp')

      expect do
        ConfCtl::Inputs::Setter.send(
          :run_nix_flake_lock!,
          dir,
          'vpsadminos',
          'github:vpsfreecz/vpsadminos/staging',
          tmpfile
        )
      end.to raise_error(ConfCtl::Error, /GitHub API rate limit was exceeded/)
    end
  end
end
