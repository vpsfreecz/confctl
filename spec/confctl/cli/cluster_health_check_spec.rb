# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe ConfCtl::Cli::Cluster do
  let(:gopts) { { color: 'never' } }
  let(:opts) { { yes: true } }
  let(:command) { described_class.new(gopts, opts, [nil]) }

  def machine(managed: true, runnable: true, checks: [])
    instance_double(
      ConfCtl::Machine,
      managed:,
      runnable?: runnable,
      health_checks: checks
    )
  end

  it 'runs checks only for runnable machines' do
    direct_check = instance_double(ConfCtl::HealthChecks::Base)
    carried_check = instance_double(ConfCtl::HealthChecks::Base)
    direct = machine(checks: [direct_check])
    carried = machine(runnable: false, checks: [carried_check])
    machines = ConfCtl::MachineList.new(
      machines: {
        'direct' => direct,
        'carried' => carried
      }
    )

    allow(command).to receive(:select_machines).with(nil).and_return(machines)

    expect(command).to receive(:run_health_checks) do |selected, run_checks|
      expect(selected.to_a).to eq([direct])
      expect(run_checks).to eq([direct_check])
      []
    end

    command.health_check
  end
end
