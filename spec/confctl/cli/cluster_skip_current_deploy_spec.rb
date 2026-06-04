# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'confctl/cli'

RSpec.describe ConfCtl::Cli::Cluster do
  let(:gopts) { { color: 'never' } }
  let(:opts) { { yes: true } }
  let(:command) { described_class.new(gopts, opts, []) }
  let(:target_toplevel) { '/nix/store/target-system' }
  let(:generation) do
    instance_double(
      ConfCtl::Generation::Build,
      name: '2026-06-04--12-00-00',
      toplevel: target_toplevel
    )
  end

  def status_with(current_toplevel:, profile_toplevel: nil)
    profile_generation =
      if profile_toplevel
        instance_double(ConfCtl::Generation::Host, toplevel: profile_toplevel)
      end

    generations =
      if profile_generation
        instance_double(ConfCtl::Generation::HostList, current: profile_generation)
      end

    instance_double(
      ConfCtl::MachineStatus,
      current_toplevel:,
      generations:
    )
  end

  def machine_with(carried:)
    instance_double(ConfCtl::Machine, carried?: carried)
  end

  def already_using_target?(action:, current_toplevel:, profile_toplevel:, carried: false)
    command.send(
      :already_using_target_generation?,
      machine_with(carried:),
      status_with(current_toplevel:, profile_toplevel:),
      generation,
      action
    )
  end

  it 'skips standalone boot and switch when runtime and profile match' do
    %w[boot switch].each do |action|
      expect(
        already_using_target?(
          action:,
          current_toplevel: target_toplevel,
          profile_toplevel: target_toplevel
        )
      ).to be(true)
    end
  end

  it 'does not skip standalone boot and switch when the profile is stale' do
    %w[boot switch].each do |action|
      expect(
        already_using_target?(
          action:,
          current_toplevel: target_toplevel,
          profile_toplevel: '/nix/store/old-system'
        )
      ).to be(false)
    end
  end

  it 'skips standalone test and dry-activate when runtime matches' do
    %w[test dry-activate].each do |action|
      expect(
        already_using_target?(
          action:,
          current_toplevel: target_toplevel,
          profile_toplevel: '/nix/store/old-system'
        )
      ).to be(true)
    end
  end

  it 'skips carried machines when their carrier-managed profile matches' do
    expect(
      already_using_target?(
        action: 'switch',
        current_toplevel: target_toplevel,
        profile_toplevel: nil,
        carried: true
      )
    ).to be(true)
  end

  it 'does not skip carried machines when their carrier-managed profile is stale' do
    expect(
      already_using_target?(
        action: 'switch',
        current_toplevel: '/nix/store/old-system',
        profile_toplevel: nil,
        carried: true
      )
    ).to be(false)
  end

  it 'does not skip when status data is unavailable' do
    expect(
      command.send(
        :already_using_target_generation?,
        machine_with(carried: false),
        status_with(current_toplevel: nil, profile_toplevel: target_toplevel),
        generation,
        'switch'
      )
    ).to be(false)
  end

  it 'does not pre-filter copy-only deployments' do
    copy_command = described_class.new(gopts, opts.merge('copy-only' => true), [])
    machine = machine_with(carried: false)
    machines = ConfCtl::MachineList.new(machines: { 'host' => machine })
    host_generations = { 'host' => generation }

    allow(copy_command).to receive(:deploy_target_statuses).and_raise('unexpected status query')

    filtered_machines, filtered_generations =
      copy_command.send(:skip_current_deploy_targets, machines, host_generations, 'switch')

    expect(filtered_machines).to equal(machines)
    expect(filtered_generations).to equal(host_generations)
  end

  it 'removes skipped hosts from deploy inputs' do
    machine = machine_with(carried: false)
    machines = ConfCtl::MachineList.new(machines: { 'host' => machine })
    host_generations = { 'host' => generation }

    allow(command).to receive(:deploy_target_statuses).and_return(
      'host' => status_with(
        current_toplevel: target_toplevel,
        profile_toplevel: target_toplevel
      )
    )

    expect do
      @filtered_machines, @filtered_generations =
        command.send(:skip_current_deploy_targets, machines, host_generations, 'switch')
    end.to output(/Skipping host: already using target generation 2026-06-04--12-00-00/).to_stdout

    expect(@filtered_machines).to be_empty
    expect(@filtered_generations).to be_empty
  end
end
