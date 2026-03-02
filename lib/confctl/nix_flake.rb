require 'json'
require_relative 'nix_build_flake'
require_relative 'nix/args'

module ConfCtl
  class NixFlake < NixLegacy
    def confctl_settings
      @confctl_settings ||= begin
        settings = nix_eval_json('.#confctl.settings', impure: false, settings: {})
        demodulify(settings)
      end
    end

    # Returns an array with options from all confctl modules
    # @return [Array]
    def module_options
      []
    end

    # Returns an array with machine fqdns
    # @return [Array<String>]
    def list_machine_fqdns
      nix_eval_json('.#confctl.machineNames')
    end

    # Return machines and their config in a hash
    # @return [Hash]
    def list_machines
      machines = nix_eval_json('.#confctl.machines')
      machines = demodulify(machines)
      refresh_machine_key_maps(machines)
      machines
    end

    # Evaluate flake installable and parse JSON
    # @param installable [String]
    # @return [Object]
    def eval_json(installable)
      nix_eval_json(installable)
    end

    # Return list of swpins channels
    # @return [Hash]
    def list_swpins_channels
      confctl_settings.fetch('swpins', {}).fetch('channels', {})
    end

    # Evaluate core swpins for host
    # @return [Hash]
    def eval_core_swpins
      {}
    end

    # Evaluate swpins for hosts
    # @param hosts [Array<String>]
    # @return [Hash] host => swpins
    def eval_host_swpins(hosts)
      plan = build_plan

      hosts.to_h do |host|
        host_plan = plan.fetch(host)
        [host, host_plan['inputs'] || {}]
      end
    end

    # Evaluate inputs info for host
    # @param host [String]
    # @return [Hash]
    def eval_inputs_info(host)
      nix_eval_json(inputs_info_installable(host))
    end

    # Evaluate inputs for host
    # @param host [String]
    # @return [Hash]
    def eval_inputs(host)
      nix_eval_json(inputs_installable(host))
    end

    # Build config.system.build.toplevel for selected hosts
    #
    # @param hosts [Array<String>]
    # @param swpin_paths [Hash]
    # @param host_swpin_specs [Hash]
    # @param time [Time]
    #
    # @yieldparam type [:build, :fetch]
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    #
    # @return [Hash<String, Generation::Build>]
    def build_attributes(hosts: [], swpin_paths: {}, host_swpin_specs: {}, time: nil, &)
      plan = build_plan
      time ||= Time.now
      ret_generations = {}
      host_plans = {}
      installables = []
      machine_keys = []

      hosts.each do |host|
        host_plan = plan.fetch(host)
        machine_key = host_plan['key'] || host_plan['machineKey'] || host_plan['flakeKey'] || machine_key_for(host)
        host_plans[host] = host_plan
        installables << ".#confctl.build.#{machine_key}.toplevel"
        installables << ".#confctl.build.#{machine_key}.autoRollback"
        machine_keys << machine_key
      end

      legacy_args = legacy_nix_path_args(hosts)
      nix_build_json(installables, legacy_nix_path_args: legacy_args, &)
      build_outputs = build_outputs_for_keys(machine_keys)
      pending_generations = {}

      hosts.each do |host|
        host_plan = host_plans[host]
        machine_key = host_plan['key'] || host_plan['machineKey'] || host_plan['flakeKey'] || machine_key_for(host)
        host_input_paths = host_plan['inputs'] || {}

        outputs = build_outputs[machine_key]
        if outputs.nil?
          raise ConfCtl::Error, "missing build outputs for #{machine_key.inspect}"
        end

        toplevel_path = outputs['toplevel']
        auto_rollback_path = outputs['autoRollback']

        if toplevel_path.nil? || auto_rollback_path.nil?
          raise ConfCtl::Error, "invalid build outputs for #{machine_key.inspect}"
        end

        host_generations = Generation::BuildList.new(host)
        generation = host_generations.find(toplevel_path, host_input_paths, mode: 'flakes')

        if generation.nil?
          pending_generations[host] = {
            host_generations: host_generations,
            machine_key: machine_key,
            toplevel_path: toplevel_path,
            auto_rollback_path: auto_rollback_path,
            host_input_paths: host_input_paths
          }
          next
        end

        host_generations.current = generation
        ret_generations[host] = generation
      end

      if pending_generations.any?
        inputs_infos = inputs_info_for_keys(pending_generations.values.map { |v| v[:machine_key] })

        pending_generations.each do |host, data|
          inputs_info = inputs_infos[data[:machine_key]] || eval_inputs_info(host)
          generation = Generation::Build.new(host)
          generation.create_flake(
            data[:toplevel_path],
            data[:auto_rollback_path],
            inputs: data[:host_input_paths],
            inputs_info: inputs_info,
            date: time
          )
          generation.save

          data[:host_generations].current = generation
          ret_generations[host] = generation
        end
      end

      ret_generations
    end

    protected

    def build_plan
      @build_plan ||= nix_eval_json('.#confctl.buildPlan')
    end

    def inputs_info_installable(host)
      ".#confctl.inputsInfo.#{machine_key_for(host)}"
    end

    def inputs_installable(host)
      ".#confctl.inputs.#{machine_key_for(host)}"
    end

    def nix_eval_json(installable, impure: nil, settings: nil, apply: nil)
      settings_for_args = settings || confctl_settings

      result = run_nix_with_fallback do |extra_experimental, no_update_lock_file|
        args_builder = nix_args(
          settings: settings_for_args,
          impure: impure,
          no_update_lock_file: no_update_lock_file
        )

        nix_eval_args(
          installable,
          args_builder: args_builder,
          extra_experimental: extra_experimental,
          apply: apply
        )
      end

      out, = result.stdout
      JSON.parse(out)
    end

    def nix_build_json(installables, legacy_nix_path_args: [], &block)
      extra_experimental = false
      no_update_lock_file = true

      loop do
        args_builder = nix_args(
          settings: confctl_settings,
          no_update_lock_file: no_update_lock_file
        )

        args = nix_build_args(
          installables,
          args_builder: args_builder,
          extra_experimental: extra_experimental,
          legacy_nix_path_args: legacy_nix_path_args
        )

        nb = NixBuildFlake.new(args, chdir: conf_dir)
        result = nb.run(&block)
        out, = result.stdout
        return JSON.parse(out)
      rescue TTY::Command::ExitError => e
        if no_update_lock_file && no_update_lock_file_error?(e.message)
          no_update_lock_file = false
          retry
        elsif !extra_experimental && experimental_error?(e.message)
          extra_experimental = true
          retry
        else
          raise
        end
      end
    end

    def run_nix_with_fallback
      extra_experimental = false
      no_update_lock_file = true

      loop do
        args = yield(extra_experimental, no_update_lock_file)

        begin
          return cmd.run(*args, chdir: conf_dir)
        rescue TTY::Command::ExitError => e
          if no_update_lock_file && no_update_lock_file_error?(e.message)
            no_update_lock_file = false
            next
          end

          if !extra_experimental && experimental_error?(e.message)
            extra_experimental = true
            next
          end

          raise
        end
      end
    end

    def nix_args(settings:, impure: nil, no_update_lock_file: true)
      ConfCtl::Nix::Args.new(
        settings: settings,
        impure: impure,
        no_update_lock_file: no_update_lock_file
      )
    end

    def nix_eval_args(installable, args_builder:, extra_experimental:, apply: nil)
      args = ['nix', 'eval', '--json']
      args.concat(
        nix_common_args(
          args_builder.eval_args,
          extra_experimental: extra_experimental
        )
      )
      if apply
        args << '--apply' << apply
      end
      args << installable
      args
    end

    def nix_build_args(installables, args_builder:, extra_experimental:, legacy_nix_path_args: [])
      args = ['--json', '--no-link']
      args.concat(
        nix_common_args(
          args_builder.build_args,
          extra_experimental: extra_experimental
        )
      )
      args.concat(legacy_nix_path_args)
      args.concat(installables)
      args
    end

    def nix_common_args(base_args, extra_experimental:)
      args = []
      if extra_experimental
        args << '--extra-experimental-features' << 'nix-command'
        args << '--extra-experimental-features' << 'flakes'
      end

      args.concat(base_args)

      args << '--show-trace' if show_trace

      if max_jobs
        args << '--option' << 'max-jobs' << max_jobs.to_s
      end

      if cores
        args << '--option' << 'cores' << cores.to_s
      end
      args
    end

    def nix_attr_string(value)
      escaped = value.to_s.gsub('\\', '\\\\').gsub('"', '\\"')
      "\"#{escaped}\""
    end

    def build_outputs_for_keys(machine_keys)
      return {} if machine_keys.empty?

      keys = machine_keys.uniq
      apply = list_to_attrs_apply_expr(keys)
      nix_eval_json('.#confctl.build', apply: apply)
    end

    def inputs_info_for_keys(machine_keys)
      return {} if machine_keys.empty?

      keys = machine_keys.uniq
      apply = list_to_attrs_apply_expr(keys)
      nix_eval_json('.#confctl.inputsInfo', apply: apply)
    end

    def inputs_for_keys(machine_keys)
      return {} if machine_keys.empty?

      keys = machine_keys.uniq
      apply = list_to_attrs_apply_expr(keys)
      nix_eval_json('.#confctl.inputs', apply: apply)
    end

    def list_to_attrs_apply_expr(keys)
      list = keys.map { |k| nix_attr_string(k) }.join(' ')
      "x: builtins.listToAttrs (map (k: { name = k; value = x.${k}; }) [ #{list} ])"
    end

    def legacy_nix_path_args(hosts)
      return [] if hosts.empty?

      args_builder = nix_args(settings: confctl_settings)
      return [] unless args_builder.legacy_nix_path?

      unless args_builder.impure?
        raise ConfCtl::Error, 'legacyNixPath requires impureEval'
      end

      merged_inputs = {}
      host_keys = {}
      hosts.each { |host| host_keys[host] = machine_key_for(host) }
      inputs_by_key = inputs_for_keys(host_keys.values)

      hosts.each do |host|
        machine_key = host_keys[host]
        inputs = inputs_by_key[machine_key] || nix_eval_json(inputs_installable(host))

        inputs.each do |name, path|
          next unless path.is_a?(String) && !path.empty?

          key = name.to_s
          if merged_inputs.has_key?(key) && merged_inputs[key] != path
            raise ConfCtl::Error,
                  "legacyNixPath requires consistent input #{key} across hosts; build hosts separately"
          end
          merged_inputs[key] = path
        end
      end

      args_builder.legacy_names.each_with_object([]) do |name, acc|
        path = merged_inputs[name.to_s]
        next unless path.is_a?(String) && !path.empty?

        acc << '-I' << "#{name}=#{path}"
      end
    end

    def refresh_machine_key_maps(machines)
      @machine_name_to_key = {}
      @machine_key_to_name = {}

      machines.each do |name, info|
        key = info['key'] || info['machineKey'] || info['flakeKey'] || name
        @machine_name_to_key[name] = key
        @machine_key_to_name[key] = name
      end
    end

    def ensure_machine_key_maps
      return if @machine_name_to_key && @machine_key_to_name

      mapping = nix_eval_json('.#confctl.machineKeys')
      @machine_name_to_key = mapping
      @machine_key_to_name = mapping.to_h { |name, key| [key, name] }
    end

    def machine_key_for(host)
      ensure_machine_key_maps

      if @machine_name_to_key.has_key?(host)
        @machine_name_to_key[host]
      elsif @machine_key_to_name.has_key?(host)
        host
      else
        raise ConfCtl::Error, "Unknown machine #{host.inspect}"
      end
    end

    def no_update_lock_file_error?(message)
      message.match?(/--no-update-lock-file/) && message.match?(/unknown|unrecognized|invalid|unsupported/i)
    end

    def experimental_error?(message)
      message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
    end
  end
end
