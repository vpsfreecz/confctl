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

    # @yieldparam [Integer] number of attempts
    # @yieldparam [Array] errors
    def run
      @started_at = Time.now
      now = @started_at
      attempt = 1

      until timeout?(now) do
        @errors.clear
        run_check
        break if successful?
        yield(attempt, errors) if block_given?
        sleep(cooldown)
        attempt += 1
        now = Time.now
      end
    end

    def successful?
      @errors.empty?
    end

    def description
      raise NotImplementedError
    end

    def message
      @errors.join('; ')
    end

    protected
    attr_reader :started_at

    def run_check
      raise NotImplementedError
    end

    def timeout?(time)
      true
    end

    def cooldown
      1
    end

    def add_error(msg)
      @errors << msg
    end
  end
end
