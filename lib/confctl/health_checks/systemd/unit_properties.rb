require 'confctl/health_checks/base'

module ConfCtl
  class HealthChecks::Systemd::UnitProperties < HealthChecks::Base
    # @param machine [Machine]
    # @param unit [String]
    # @param property_checks [Array<HealthChecks::Systemd::PropertyCheck>]
    def initialize(machine, unit, property_checks)
      super(machine)
      @unit = unit
      @property_checks = property_checks
    end

    def run
      mc = MachineControl.new(machine)
      result = mc.execute!('systemctl', 'show', @unit)

      if result.failure?
        add_error("systemctl show #{@unit} failed with #{result.status}")
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

    def message
      "#{@unit}: #{super}"
    end
  end
end
