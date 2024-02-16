module ConfCtl
  class Machine
    attr_reader :name, :safe_name, :managed, :spin, :opts

    # @param opts [Hash]
    def initialize(opts)
      @opts = opts
      @name = opts['name']
      @safe_name = opts['name'].gsub('/', ':')
      @managed = opts['managed']
      @spin = opts['spin']
    end

    def target_host
      (opts['host'] && opts['host']['target']) || name
    end

    def localhost?
      target_host == 'localhost'
    end

    def nix_paths
      opts['nix']['nixPath'].to_h do |v|
        eq = v.index('=')
        raise "'#{v}' is not a valid nix path entry " if eq.nil?

        [v[0..eq - 1], v[eq + 1..]]
      end
    end

    def health_checks
      return @health_checks if @health_checks

      @health_checks = []

      opts['healthChecks'].each do |type, checks|
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
        get(opts, key.split('.'))
      elsif key == 'checks'
        health_checks.length
      else
        opts[key]
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
