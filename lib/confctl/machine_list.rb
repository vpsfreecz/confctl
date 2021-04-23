require 'json'

module ConfCtl
  class MachineList
    # @param opts [Hash]
    # @option opts [Boolean] :show_trace
    # @option opts [Boolean] :machines
    def initialize(opts = {})
      @opts = opts
      @machines = opts[:machines] || parse(extract)
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    def each(&block)
      machines.each(&block)
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    # @return [MachineList]
    def select(&block)
      self.class.new(machines: machines.select(&block))
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    # @return [Array]
    def map(&block)
      machines.map(&block)
    end

    # @return [MachineList]
    def managed
      select { |host, dep| dep.managed }
    end

    # @return [MachineList]
    def unmanaged
      select { |host, dep| !dep.managed }
    end

    # @param host [String]
    def [](host)
      @machines[host]
    end

    # @return [Integer]
    def length
      @machines.length
    end

    protected
    attr_reader :opts, :machines

    def extract
      nix = Nix.new
      nix.list_machines
    end

    def parse(data)
      Hash[data.map do |host, info|
        [host, Deployment.new(info)]
      end]
    end
  end
end
