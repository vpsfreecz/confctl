module ConfCtl
  class HealthChecks::Base
    # @return [Machine]
    attr_reader :machine

    # @return [Array<String>]
    attr_reader :errors

    # @param machine [Machine]
    def initialize(machine)
      @machine = machine
      @errors = []
    end

    def run
      raise NotImplementedError
    end

    def successful?
      @errors.empty?
    end

    def message
      @errors.join('; ')
    end

    protected
    def add_error(msg)
      @errors << msg
    end
  end
end
