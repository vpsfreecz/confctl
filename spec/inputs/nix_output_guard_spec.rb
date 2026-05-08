# frozen_string_literal: true

require 'spec_helper'
require 'confctl/exceptions'
require 'confctl/inputs/nix_output_guard'
require 'tty-command'

RSpec.describe ConfCtl::Inputs::NixOutputGuard do
  def command_result(stderr)
    TTY::Command::Result.new(0, '', stderr)
  end

  it 'raises when Nix falls back to cached GitHub metadata after rate limiting' do
    stderr = <<~ERR
      warning: error: unable to download 'https://api.github.com/repos/vpsfreecz/vpsadminos/commits/staging': HTTP error 403

             response body:

             {"message":"API rate limit exceeded for 192.0.2.1."}; using cached version
    ERR

    expect do
      described_class.check!(command_result(stderr))
    end.to raise_error(ConfCtl::Error, /GitHub API rate limit was exceeded/)
  end

  it 'raises when Nix falls back to cached GitHub metadata for another API error' do
    stderr = <<~ERR
      warning: error: unable to download 'https://api.github.com/repos/example/project/commits/main': HTTP error 503
      ; using cached version
    ERR

    expect do
      described_class.check!(command_result(stderr))
    end.to raise_error(ConfCtl::Error, /used a cached version/)
  end

  it 'does not raise for unrelated Nix warnings' do
    stderr = "warning: '--update-input' is a deprecated alias for 'flake update'\n"

    expect do
      described_class.check!(command_result(stderr))
    end.not_to raise_error
  end
end
