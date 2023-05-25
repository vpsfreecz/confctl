require 'confctl/health_checks/base'

module ConfCtl
  class HealthChecks::Systemd::Properties < HealthChecks::Base
    # @param machine [Machine]
    # @param pattern [String, nil]
    # @param property_checks [Array<HealthChecks::Systemd::PropertyCheck>]
    def initialize(machine, pattern: nil, property_checks:)
      super(machine)
      @pattern = pattern
      @property_checks = property_checks
      @shortest_timeout = property_checks.inject(nil) do |acc, check|
        if acc.nil? || check.timeout < acc
          check.timeout
        else
          acc
        end
      end
      @shortest_cooldown = property_checks.inject(nil) do |acc, check|
        if acc.nil? || check.cooldown < acc
          check.cooldown
        else
          acc
        end
      end
    end

    def description
      ret = ''

      if @pattern
        ret << @pattern << ': '
      else
        ret << 'systemd: '
      end

      ret << @property_checks.map(&:to_s).join(', ')
      ret
    end

    def message
      if @pattern
        "#{@pattern}: #{super}"
      else
        super
      end
    end

    protected
    def run_check
      mc = MachineControl.new(machine)
      cmd = %w(systemctl show)
      cmd << @pattern if @pattern
      result = mc.execute!(*cmd)

      if result.failure?
        add_error("#{cmd.join(' ')} failed with #{result.status}")
        return
      end

      properties = HealthChecks::Systemd::PropertyList.from_enumerator(result.each)

      @property_checks.each do |check|
        v = properties[check.property]

        if v.nil?
          add_error("property #{check.property.inspect} not found")
          next
        end

        unless check.check(v)
          add_error("#{check.property}=#{v}, expected #{check.value}")
        end
      end
    end

    def timeout?(time)
      started_at + @shortest_timeout < time
    end

    def cooldown
      @shortest_cooldown
    end
  end
end
