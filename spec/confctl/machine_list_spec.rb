# frozen_string_literal: true

require 'spec_helper'
require 'confctl'

RSpec.describe ConfCtl::MachineList do
  def machine(managed: true, runnable: true, checks: [])
    instance_double(
      ConfCtl::Machine,
      managed:,
      runnable?: runnable,
      health_checks: checks
    )
  end

  it 'selects only runnable machines' do
    direct = machine
    carried = machine(runnable: false)
    no_target = machine(runnable: false)

    list = described_class.new(
      machines: {
        'direct' => direct,
        'carried' => carried,
        'no-target' => no_target
      }
    )

    expect(list.runnable.to_a).to eq([direct])
  end

  it 'returns health checks only from runnable machines' do
    direct_check = instance_double(ConfCtl::HealthChecks::Base)
    carried_check = instance_double(ConfCtl::HealthChecks::Base)
    direct = machine(checks: [direct_check])
    carried = machine(runnable: false, checks: [carried_check])

    list = described_class.new(
      machines: {
        'direct' => direct,
        'carried' => carried
      }
    )

    expect(list.health_checks).to eq([direct_check])
  end
end
