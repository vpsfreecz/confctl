require 'json'

module ConfCtl
  class MachineList
    # @param machine [Machine]
    # @return [MachineList]
    def self.from_machine(machine)
      new(machines: { machine.name => machine })
    end

    # @param opts [Hash]
    # @option opts [Boolean] :show_trace
    # @option opts [MachineList] :machines
    def initialize(opts = {})
      @opts = opts
      @machines = opts[:machines] || parse(extract)
    end

    # @yieldparam [String] host
    # @yieldparam [Machine] machine
    def each(&)
      machines.each(&)
    end

    # @yieldparam [String] host
    def each_key(&)
      machines.each_key(&)
    end

    # @yieldparam [Machine] machine
    def each_value(&)
      machines.each_value(&)
    end

    # @yieldparam [String] host
    # @yieldparam [Machine] machine
    # @return [MachineList]
    def select(&)
      self.class.new(machines: machines.select(&))
    end

    # @yieldparam [String] host
    # @yieldparam [Machine] machine
    # @return [Array]
    def map(&)
      machines.map(&)
    end

    # @yieldparam [Machine] machine
    # @return [Hash]
    def transform_values(&)
      machines.transform_values(&)
    end

    # @return [MachineList]
    def managed
      select { |_host, machine| machine.managed }
    end

    # @return [MachineList]
    def unmanaged
      select { |_host, machine| !machine.managed }
    end

    # @return [MachineList]
    def runnable
      select { |_host, machine| !machine.carried? }
    end

    # @param host [String]
    def [](host)
      @machines[host]
    end

    # @return [Machine, nil]
    def first
      @machines.each_value.first
    end

    # @return [Integer]
    def length
      @machines.length
    end

    def empty?
      @machines.empty?
    end

    def any?
      !empty?
    end

    # @return [Array<HealthChecks::Base>]
    def health_checks
      checks = []

      machines.each_value do |machine|
        next if machine.carried?

        checks.concat(machine.health_checks)
      end

      checks
    end

    protected

    attr_reader :opts, :machines

    def extract
      nix = Nix.new
      nix.list_machines
    end

    def parse(data)
      data.transform_values do |info|
        Machine.new(info, machine_list: self)
      end
    end
  end
end
