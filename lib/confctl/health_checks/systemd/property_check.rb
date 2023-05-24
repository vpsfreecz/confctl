module ConfCtl
  class HealthChecks::Systemd::PropertyCheck
    # @return [String]
    attr_reader :property

    # @return [String]
    attr_reader :value

    def initialize(opts)
      @property = opts['property']
      @value = opts['value']
    end

    # @return [Boolean]
    def check(v)
      @value == v
    end
  end
end
