require 'json'

module ConfCtl
  class MachineList
    # @param machine [Machine]
    # @return [MachineList]
    def self.from_machine(machine)
      new(machines: {machine.name => machine})
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
    def each(&block)
      machines.each(&block)
    end

    # @yieldparam [String] host
    # @yieldparam [Machine] machine
    # @return [MachineList]
    def select(&block)
      self.class.new(machines: machines.select(&block))
    end

    # @yieldparam [String] host
    # @yieldparam [Machine] machine
    # @return [Array]
    def map(&block)
      machines.map(&block)
    end

    # @return [MachineList]
    def managed
      select { |host, machine| machine.managed }
    end

    # @return [MachineList]
    def unmanaged
      select { |host, machine| !machine.managed }
    end

    # @param host [String]
    def [](host)
      @machines[host]
    end

    # @return [Machine]
    def get_one
      @machines.each { |_, machine| return machine }
      nil
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

      machines.each do |host, machine|
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
      Hash[data.map do |host, info|
        [host, Machine.new(info)]
      end]
    end
  end
end
