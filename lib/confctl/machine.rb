module ConfCtl
  class Machine
    CarriedMachine = Struct.new(
      :carrier,
      :name,
      :alias,
      :attribute,
      keyword_init: true
    )

    attr_reader :name, :safe_name, :managed, :spin, :carrier_name, :meta

    # @param opts [Hash]
    def initialize(opts)
      @meta = opts['metaConfig']
      @name = opts['name']
      @safe_name = name.gsub('/', ':')
      @managed = meta['managed']
      @spin = meta['spin']
      @is_carrier = meta.fetch('carrier', {}).fetch('enable', false)
      @carrier_name = opts['carrier']
    end

    # True if this machine carries other machines
    def carrier?
      @is_carrier
    end

    # @return [Array<CarriedMachine>]
    def carried_machines
      meta.fetch('carrier', {}).fetch('machines', []).map do |m|
        CarriedMachine.new(
          carrier: self,
          name: m['machine'],
          alias: m['alias'] || m['machine'],
          attribute: m['attribute']
        )
      end
    end

    def target_host
      meta.fetch('host', {}).fetch('target', name)
    end

    def localhost?
      target_host == 'localhost'
    end

    def nix_paths
      meta['nix']['nixPath'].to_h do |v|
        eq = v.index('=')
        raise "'#{v}' is not a valid nix path entry " if eq.nil?

        [v[0..eq - 1], v[eq + 1..]]
      end
    end

    def health_checks
      return @health_checks if @health_checks

      @health_checks = []

      meta['healthChecks'].each do |type, checks|
        case type
        when 'systemd'
          next if !checks['enable'] || spin != 'nixos'

          if checks['systemProperties'].any?
            @health_checks << HealthChecks::Systemd::Properties.new(
              self,
              property_checks: checks['systemProperties'].map do |v|
                HealthChecks::Systemd::PropertyCheck.new(v)
              end
            )
          end

          checks['unitProperties'].each do |unit_name, prop_checks|
            health_checks << HealthChecks::Systemd::Properties.new(
              self,
              pattern: unit_name,
              property_checks: prop_checks.map do |v|
                HealthChecks::Systemd::PropertyCheck.new(v)
              end
            )
          end

        when 'builderCommands', 'machineCommands'
          checks.each do |cmd|
            health_checks << HealthChecks::RunCommand.new(
              self,
              HealthChecks::RunCommand::Command.new(self, cmd),
              remote: type == 'machineCommands'
            )
          end

        else
          raise "Unsupported health-check type #{type.inspect}"
        end
      end

      @health_checks
    end

    def [](key)
      if key.index('.')
        get(meta, key.split('.'))
      elsif key == 'name'
        name
      elsif key == 'checks'
        health_checks.length
      else
        meta[key]
      end
    end

    def to_s
      name
    end

    protected

    def get(hash, keys)
      k = keys.shift

      return unless hash.has_key?(k)

      if keys.empty?
        hash[k]
      elsif hash[k].nil?
        nil
      else
        get(hash[k], keys)
      end
    end
  end
end
