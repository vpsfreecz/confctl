require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/nix'
require 'confctl/pins/setter'
require 'confctl/pins/updater'

module ConfCtl::Cli
  class Pins::Machines < Command
    def update
      ensure_flake_config!
      require_args!('machine', 'role')

      machine_name = args[0]
      role = args[1]

      pins_info = ConfCtl::Nix.new.eval_pins_info(machine_name)
      unless pins_info.is_a?(Hash)
        raise ConfCtl::Error, "pins info unavailable for machine '#{machine_name}'"
      end

      role_info = pins_info[role] || pins_info[role.to_s] || pins_info[role.to_sym]
      input = role_info.is_a?(Hash) ? (role_info['input'] || role_info[:input]) : nil

      raise ConfCtl::Error, "machine '#{machine_name}' has no role '#{role}'" unless input

      ConfCtl::Pins::Updater.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: [input],
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      lock = ConfCtl::FlakeLock.load(File.join(ConfCtl::ConfDir.path, 'flake.lock'))
      info = lock.input_info(input)
      rev = info[:short_rev] || info[:rev] || '-'
      puts "Updating #{role} in #{machine_name} -> #{rev}"
    end

    def set
      ensure_flake_config!
      require_args!('machine', 'role', 'rev')

      machine_name = args[0]
      role = args[1]
      rev = args[2]

      pins_info = ConfCtl::Nix.new.eval_pins_info(machine_name)
      unless pins_info.is_a?(Hash)
        raise ConfCtl::Error, "pins info unavailable for machine '#{machine_name}'"
      end

      role_info = pins_info[role] || pins_info[role.to_s] || pins_info[role.to_sym]
      input = role_info.is_a?(Hash) ? (role_info['input'] || role_info[:input]) : nil

      raise ConfCtl::Error, "machine '#{machine_name}' has no role '#{role}'" unless input

      ConfCtl::Pins::Setter.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: [input],
        rev: rev,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )
      puts "Configuring #{role} in #{machine_name} -> #{rev}"
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'confctl pins machine is available only in flake configs'
    end
  end
end
