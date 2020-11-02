require 'json'

module ConfCtl
  class Deployments
    # @param opts [Hash]
    # @option opts [Boolean] :show_trace
    # @option opts [Boolean] :deployments
    def initialize(opts = {})
      @opts = opts
      @deployments = opts[:deployments] || parse(extract)
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    def each(&block)
      deployments.each(&block)
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    # @return [Deployments]
    def select(&block)
      self.class.new(deployments: deployments.select(&block))
    end

    # @yieldparam [String] host
    # @yieldparam [Deployment] deployment
    # @return [Array]
    def map(&block)
      deployments.map(&block)
    end

    # @return [Deployments]
    def managed
      select { |host, dep| dep.managed }
    end

    # @return [Deployments]
    def unmanaged
      select { |host, dep| !dep.managed }
    end

    # @param host [String]
    def [](host)
      @deployments[host]
    end

    protected
    attr_reader :opts, :deployments

    def extract
      nix = Nix.new
      nix.list_deployments
    end

    def parse(data)
      Hash[data.map do |host, info|
        [host, Deployment.new(info)]
      end]
    end
  end
end
