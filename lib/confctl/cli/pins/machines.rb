require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/nix'
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

      puts "Machine: #{machine_name}"
      puts "Role: #{role}"
      puts "Input: #{input}"

      res = ConfCtl::Pins::Updater.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: [input],
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      puts(res[:changed] ? "Updated #{input}." : 'No changes.')
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'confctl pins machine is available only in flake configs'
    end
  end
end
