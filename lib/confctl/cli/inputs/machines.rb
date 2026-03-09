require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/nix'
require 'confctl/inputs/setter'
require 'confctl/inputs/updater'

module ConfCtl::Cli
  class Inputs::Machines < Command
    def update
      ensure_flake_config!
      require_args!('machine', 'role')

      machine_name = args[0]
      role = args[1]

      inputs_info = ConfCtl::Nix.new.eval_inputs_info(machine_name)
      unless inputs_info.is_a?(Hash)
        raise ConfCtl::Error, "inputs info unavailable for machine '#{machine_name}'"
      end

      role_info = inputs_info[role] || inputs_info[role.to_s] || inputs_info[role.to_sym]
      input = role_info.is_a?(Hash) ? (role_info['input'] || role_info[:input]) : nil

      raise ConfCtl::Error, "machine '#{machine_name}' has no role '#{role}'" unless input

      ConfCtl::Inputs::Updater.run!(
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

      inputs_info = ConfCtl::Nix.new.eval_inputs_info(machine_name)
      unless inputs_info.is_a?(Hash)
        raise ConfCtl::Error, "inputs info unavailable for machine '#{machine_name}'"
      end

      role_info = inputs_info[role] || inputs_info[role.to_s] || inputs_info[role.to_sym]
      input = role_info.is_a?(Hash) ? (role_info['input'] || role_info[:input]) : nil

      raise ConfCtl::Error, "machine '#{machine_name}' has no role '#{role}'" unless input

      ConfCtl::Inputs::Setter.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: [input],
        rev: rev,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      lock = ConfCtl::FlakeLock.load(File.join(ConfCtl::ConfDir.path, 'flake.lock'))
      info = lock.input_info(input)
      resolved_rev = info[:short_rev] || info[:rev] || '-'
      puts "Configuring #{role} in #{machine_name} -> #{resolved_rev}"
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'confctl inputs machine is available only in flake configs'
    end
  end
end
