# frozen_string_literal: true

require 'spec_helper'
require 'confctl'
require 'shellwords'

RSpec.describe ConfCtl::MachineControl do
  let(:runner_class) do
    Class.new do
      attr_reader :calls, :result

      def initialize
        @calls = []
        @result = Object.new
      end

      def run(*args, **kwargs, &)
        calls << { method: :run, args:, kwargs:, block: block_given? }
        result
      end

      def run!(*args, **kwargs, &)
        calls << { method: :run!, args:, kwargs:, block: block_given? }
        result
      end
    end
  end

  let(:runner) { runner_class.new }

  before do
    allow(ConfCtl::SystemCommand).to receive(:new).and_return(runner)
    allow(ENV).to receive(:fetch).and_call_original
    allow(ENV).to receive(:fetch).with('CONFCTL_SSH_CONFIG', nil).and_return(nil)
  end

  def machine(localhost:, target_host: 'web.int.vpsfree.cz', target_port: 22)
    instance_double(
      ConfCtl::Machine,
      localhost?: localhost,
      target_host:,
      target_port:
    )
  end

  it 'passes localhost commands as literal argv' do
    command = %w[printf %s] + ['Host: vpsfree.cz']

    ret = described_class.new(machine(localhost: true)).execute(*command)

    expect(ret).to equal(runner.result)
    expect(runner.calls).to contain_exactly(
      method: :run,
      args: command,
      kwargs: {},
      block: false
    )
  end

  it 'shell-quotes remote argv into one ssh command argument' do
    command = [
      'printf',
      '%s',
      'Host: vpsfree.cz',
      "quote'arg",
      '',
      'semi;colon',
      'dollar$arg'
    ]

    ret = described_class.new(machine(localhost: false, target_port: 2222)).execute!(
      *command,
      input: 'stdin'
    )

    expect(ret).to equal(runner.result)
    expect(runner.calls).to contain_exactly(
      method: :run!,
      args: [
        'ssh',
        '-l',
        'root',
        '-p',
        '2222',
        'web.int.vpsfree.cz',
        Shellwords.join(command)
      ],
      kwargs: { input: 'stdin' },
      block: false
    )
  end

  it 'keeps extra ssh options before the target host' do
    mc = described_class.new(machine(localhost: false))

    mc.send(:with_ssh_opts, '-o', 'ConnectTimeout=3') do
      mc.execute!('true')
    end

    expect(runner.calls.first[:args]).to eq(
      [
        'ssh',
        '-l',
        'root',
        '-o',
        'ConnectTimeout=3',
        'web.int.vpsfree.cz',
        'true'
      ]
    )
  end
end
