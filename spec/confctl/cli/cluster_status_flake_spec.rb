# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe ConfCtl::Cli::Cluster do
  let(:gopts) { { color: 'never' } }
  let(:opts) { { yes: true, generation: 'none' } }
  let(:command) { described_class.new(gopts, opts, [nil]) }
  let(:host) { 'cz.vpsfree/vpsadmin/int.api1' }
  let(:machine) do
    instance_double(
      'ConfCtl::Machine',
      managed: true,
      target_host: 'int.api1',
      carried?: false
    )
  end
  let(:machines) { ConfCtl::MachineList.new(machines: { host => machine }) }
  let(:status_class) do
    Struct.new(
      :uptime,
      :inputs_info,
      :target_inputs_info,
      :target_toplevel,
      :current_toplevel,
      :generations
    ) do
      def query(**)
        nil
      end
    end
  end
  let(:status) do
    status_class.new(
      31.7 * 24 * 60 * 60,
      {
        'nixpkgs' => { 'rev' => '71caefce01234567', 'shortRev' => '71caefce' },
        'vpsadmin' => { 'rev' => 'cb29516601234567', 'shortRev' => 'cb295166' }
      },
      nil,
      nil,
      nil,
      nil
    )
  end
  let(:nix) { instance_double(ConfCtl::Nix) }
  let(:build_generations) { instance_double(ConfCtl::Generation::BuildList, count: 29) }

  before do
    allow(command).to receive(:select_machines).with(nil).and_return(machines)
    allow(ConfCtl::MachineStatus).to receive(:new).with(machine).and_return(status)
    allow(ConfCtl::Nix).to receive(:new).and_return(nix)
    allow(nix).to receive(:eval_inputs_info).with(host).and_return(
      'nixpkgs' => { 'rev' => 'fea3b36789abcdef', 'shortRev' => 'fea3b367' },
      'vpsadmin' => { 'rev' => 'cb29516601234567', 'shortRev' => 'cb295166' }
    )
    allow(ConfCtl::Generation::BuildList).to receive(:new).with(host).and_return(build_generations)
  end

  it 'shows deployed input revisions without target arrows' do
    captured_rows = nil
    captured_cols = nil

    allow(ConfCtl::Cli::OutputFormatter).to receive(:print) do |rows, cols, **|
      captured_rows = rows
      captured_cols = cols
    end

    command.status_flake

    expect(captured_cols).to include('nixpkgs', 'vpsadmin')

    row = captured_rows.fetch(0)

    expect(row['status'].to_s).to eq('outdated')
    expect(row['nixpkgs'].to_s).to eq('71caefce')
    expect(row['vpsadmin'].to_s).to eq('cb295166')
    expect(row['nixpkgs'].to_s).not_to include('->')
    expect(row['vpsadmin'].to_s).not_to include('->')
  end
end
