module ConfCtl
  class Deployment
    attr_reader :name, :safe_name, :managed, :spin, :opts

    # @param opts [Hash]
    def initialize(opts)
      @opts = opts
      @name = opts['name']
      @safe_name = opts['name'].gsub(/\//, ':')
      @managed = opts['managed']
      @spin = opts['spin']
    end

    def target_host
      (opts['host'] && opts['host']['target']) || name
    end

    def [](key)
      opts[key]
    end

    def to_s
      name
    end
  end
end
