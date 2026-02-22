require 'forwardable'
require_relative 'config_type'
require_relative 'nix_legacy'
require_relative 'nix_flake'

module ConfCtl
  class Nix
    extend Forwardable

    # Create a new instance without access to {ConfCtl::Settings}, i.e. when
    # called outside of cluster configuration directory.
    # @return [Nix]
    def self.stateless(show_trace: false, max_jobs: 'auto')
      new(show_trace: show_trace, max_jobs: max_jobs)
    end

    def initialize(conf_dir: nil, show_trace: false, max_jobs: nil, cores: nil)
      @conf_dir = conf_dir || ConfDir.path
      impl_klass = ConfigType.flake?(@conf_dir) ? NixFlake : NixLegacy
      @impl = impl_klass.new(conf_dir: @conf_dir, show_trace: show_trace, max_jobs: max_jobs, cores: cores)
    end

    def_delegators :@impl,
                   :confctl_settings,
                   :module_options,
                   :list_machine_fqdns,
                   :list_machines,
                   :build_attributes,
                   :copy,
                   :activate,
                   :activate_with_rollback,
                   :set_profile,
                   :set_carried_profile,
                   :run_command_in_shell,
                   :collect_garbage,
                   :list_swpins_channels,
                   :eval_core_swpins,
                   :eval_host_swpins,
                   :eval_inputs_info,
                   :eval_inputs,
                   :eval_json
  end
end
