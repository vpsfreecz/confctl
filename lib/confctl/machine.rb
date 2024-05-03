module ConfCtl
  class Machine
    CarriedMachine = Struct.new(
      :carrier,
      :name,
      :alias,
      :attribute,
      keyword_init: true
    )

    # @return [String]
    attr_reader :name

    # @return [String]
    attr_reader :safe_name

    # @return [Boolean]
    attr_reader :managed

    # @return [String]
    attr_reader :spin

    # @return [String]
    attr_reader :carrier_name

    # @return [String]
    attr_reader :cluster_name

    # @return [String]
    attr_reader :safe_cluster_name

    # Alias for this machine on the carrier
    # @return [String]
    attr_reader :carried_alias

    # Alias for this machine on the carrier
    # @return [String]
    attr_reader :safe_carried_alias

    # @return [Hash] machine metadata
    attr_reader :meta

    # @param opts [Hash]
    # @param machine_list [MachineList]
    def initialize(opts, machine_list:)
      @meta = opts['metaConfig']
      @name = opts['name']
      @safe_name = name.gsub('/', ':')
      @managed = meta['managed']
      @spin = meta['spin']
      @is_carrier = meta.fetch('carrier', {}).fetch('enable', false)
      @carrier_name = opts['carrier']
      @cluster_name = opts['clusterName']
      @safe_cluster_name = cluster_name.gsub('/', ':')
      @carried_alias = opts['alias'] || @cluster_name
      @safe_carried_alias = @carried_alias.gsub('/', ':')
      @machine_list = machine_list
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

    # True if this machine is on a carrier
    def carried?
      !@carrier_name.nil?
    end

    # @return [Machine] carrier
    def carrier_machine
      carrier = @machine_list[@carrier_name]

      if carrier.nil?
        raise "Carrier #{@carrier_name} not found in machine list"
      end

      carrier
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
