# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe 'flake input set output' do
  let(:gopts) { { color: 'never' } }
  let(:common_opts) { { commit: false, changelog: true, downgrade: false, editor: false } }
  let(:lock) { instance_double(ConfCtl::FlakeLock) }
  let(:resolved_info) { { short_rev: '5361648e', rev: '5361648e4a29f58b5c208d3f185d95cd0a43fe54' } }

  around do |example|
    old_tty = ENV['CONFCTL_TTY']
    ENV['CONFCTL_TTY'] = '0'
    example.run
  ensure
    ENV['CONFCTL_TTY'] = old_tty
  end

  before do
    allow(ConfCtl::ConfDir).to receive(:path).and_return('/conf')
    allow(ConfCtl::ConfigType).to receive(:flake?).with('/conf').and_return(true)
    allow(ConfCtl::Inputs::Setter).to receive(:run!).and_return(changed: true, changes: [])
    allow(ConfCtl::FlakeLock).to receive(:load).with('/conf/flake.lock').and_return(lock)
    allow(lock).to receive(:input_info).with('vpsadmin-input').and_return(resolved_info)
  end

  it 'prints the resolved locked revision for channel set' do
    command = ConfCtl::Cli::Inputs::Channels.new(
      gopts,
      common_opts.merge(allow_shared: false),
      %w[vpsadmin vpsadmin devel]
    )

    allow(command).to receive(:eval_channels).and_return(
      'vpsadmin' => { 'vpsadmin' => 'vpsadmin-input' }
    )

    expect do
      command.set
    end.to output("Configuring vpsadmin in vpsadmin -> 5361648e\n").to_stdout

    expect(ConfCtl::Inputs::Setter).to have_received(:run!).with(
      hash_including(conf_dir: '/conf', inputs: ['vpsadmin-input'], rev: 'devel')
    )
  end

  it 'prints the resolved locked revision for machine set' do
    nix = instance_double(ConfCtl::Nix)
    allow(ConfCtl::Nix).to receive(:new).and_return(nix)
    allow(nix).to receive(:eval_inputs_info).with('build.vpsfree.cz').and_return(
      'vpsadmin' => { 'input' => 'vpsadmin-input' }
    )

    command = ConfCtl::Cli::Inputs::Machines.new(
      gopts,
      common_opts,
      %w[build.vpsfree.cz vpsadmin devel]
    )

    expect do
      command.set
    end.to output("Configuring vpsadmin in build.vpsfree.cz -> 5361648e\n").to_stdout

    expect(ConfCtl::Inputs::Setter).to have_received(:run!).with(
      hash_including(conf_dir: '/conf', inputs: ['vpsadmin-input'], rev: 'devel')
    )
  end
end
