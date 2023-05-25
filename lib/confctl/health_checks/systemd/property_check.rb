module ConfCtl
  class HealthChecks::Systemd::PropertyCheck
    # @return [String]
    attr_reader :property

    # @return [String]
    attr_reader :value

    # @return [Integer]
    attr_reader :timeout

    # @return [Integer]
    attr_reader :cooldown

    def initialize(opts)
      @property = opts['property']
      @value = opts['value']
      @timeout = opts['timeout']
      @cooldown = opts['cooldown']
    end

    # @return [Boolean]
    def check(v)
      @value == v
    end

    def to_s
      "#{property}=#{value}"
    end
  end
end
