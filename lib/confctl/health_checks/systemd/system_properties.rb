require 'confctl/health_checks/base'

module ConfCtl
  class HealthChecks::Systemd::SystemProperties < HealthChecks::Base
    # @param machine [Machine]
    # @param property_checks [Array<HealthChecks::Systemd::PropertyCheck>]
    def initialize(machine, property_checks)
      super(machine)
      @property_checks = property_checks
    end

    def run
      mc = MachineControl.new(machine)
      result = mc.execute!('systemctl', 'show')

      if result.failure?
        add_error("systemctl show failed with #{result.status}")
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
  end
end
