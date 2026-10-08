require 'confctl/utils/file'
require 'json'
require 'securerandom'
require 'shellwords'
require_relative 'nix_build_flake'
require_relative 'nix/args'

module ConfCtl
  class Nix
    # Create a new instance without access to {ConfCtl::Settings}, i.e. when
    # called outside of cluster configuration directory.
    # @return [Nix]
    def self.stateless(show_trace: false, max_jobs: 'auto')
      new(show_trace: show_trace, max_jobs: max_jobs)
    end

    include Utils::File

    def initialize(conf_dir: nil, show_trace: false, max_jobs: nil, cores: nil)
      @conf_dir = conf_dir || ConfDir.path
      @show_trace = show_trace
      @max_jobs = max_jobs || Settings.instance.max_jobs
      @cores = cores
      @cmd = SystemCommand.new
    end

    def confctl_settings
      @confctl_settings ||= begin
        settings = nix_eval_json('.#confctl.settings', impure: false, settings: {})
        demodulify(settings)
      end
    end

    # Returns an array with options from all confctl modules
    # @return [Array]
    def module_options
      nix_eval_json('.#confctl.moduleOptions')
    end

    def documentation_options
      nix_eval_json("#{ConfCtl.root}#moduleOptions", impure: false, settings: {}, configuration: false)
    end

    # Returns an array with machine fqdns
    # @return [Array<String>]
    def list_machine_fqdns
      nix_eval_json('.#confctl.machineNames')
    end

    # Return machines and their config in a hash
    # @return [Hash]
    def list_machines
      machines_json = nix_build_output_path('.#confctl.machinesJson')
      machines = JSON.parse(File.read(machines_json))
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

    # Evaluate input paths for hosts
    # @param hosts [Array<String>]
    # @return [Hash] host => inputs
    def eval_host_inputs(hosts)
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
    # @param time [Time]
    #
    # @yieldparam type [:build, :fetch]
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    #
    # @return [Hash<String, Generation::Build>]
    def build_attributes(hosts: [], time: nil, &)
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
        generation = host_generations.find(toplevel_path, host_input_paths)

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

    # @param paths [Array<String>]
    #
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    #
    # @return [Boolean]
    def copy(machine, paths, &)
      if machine.localhost?
        true
      elsif machine.carried?
        carrier = machine.carrier_machine
        cp = NixCopy.new(carrier.target_host, paths, port: carrier.target_port)
        cp.run!(&).success?
      else
        cp = NixCopy.new(machine.target_host, paths, port: machine.target_port)
        cp.run!(&).success?
      end
    end

    # @param machine [Machine]
    # @param generation [Generation::Build]
    # @param action [String]
    # @return [Boolean]
    def activate(machine, generation, action)
      args = [File.join(generation.toplevel, 'bin/switch-to-configuration'), action]

      MachineControl.new(machine).execute!(*args).success?
    end

    # @param machine [Machine]
    # @param generation [Generation::Build]
    # @param action [String]
    # @return [Boolean]
    def activate_with_rollback(machine, generation, action)
      check_file = File.join('/run', "confctl-confirm-#{SecureRandom.hex(3)}")
      timeout = machine['autoRollback']['timeout']
      logger = NullLogger.new

      args = [
        generation.auto_rollback,
        '-t', timeout,
        generation.toplevel,
        action,
        check_file
      ]

      activation_success = nil

      activation_thread = Thread.new do
        activation_success = MachineControl.new(machine).execute!(*args).success?
      end

      # Wait for the configuration to be switched
      t = Time.now
      mc = MachineControl.new(machine, logger:)

      loop do
        out, = mc.execute!('sh', '-c', "cat #{Shellwords.escape(check_file)} 2>/dev/null")
        stripped = out.strip
        break if stripped == 'switched' || ((t + timeout + 10) < Time.now && stripped != 'switching')

        sleep(1)
      end

      # Confirm it
      confirm_cmd = ['sh', '-c', "echo confirmed > #{Shellwords.escape(check_file)}"]

      10.times do
        break if mc.execute!(*confirm_cmd).success?
      end

      activation_thread.join
      activation_success
    end

    # @param machine [Machine]
    # @param toplevel [String]
    # @return [Boolean]
    def set_profile(machine, toplevel)
      args = [
        'nix-env',
        '-p', machine.profile,
        '--set', toplevel
      ]

      MachineControl.new(machine).execute!(*args).success?
    end

    # @param machine [Machine]
    # @param toplevel [String]
    # @return [Boolean]
    def set_carried_profile(machine, toplevel)
      args = [
        'carrier-env',
        '-p', machine.profile,
        '--set', toplevel
      ]

      MachineControl.new(machine.carrier_machine).execute!(*args).success?
    end

    # @param packages [Array<String>]
    # @param command [String]
    # @return [Boolean]
    def run_command_in_shell(packages: [], command: nil)
      args = ['nix-shell']

      if packages.any?
        args << '-p'
        args.concat(packages)
      end

      args << '--command'
      args << command

      cmd.run!(*args, env: { 'shellHook' => nil }).success?
    end

    # @param machine [Machine]
    # @yieldparam progress [NixCollectGarbage::Progress]
    # @return [Boolean]
    def collect_garbage(machine, &)
      gc = NixCollectGarbage.new(machine)
      gc.run!(&).success?
    end

    protected

    attr_reader :conf_dir, :show_trace, :max_jobs, :cores, :cmd

    def demodulify(value)
      if value.is_a?(Array)
        value.each { |item| demodulify(item) }
      elsif value.is_a?(Hash)
        value.delete('_module')
        value.each_value { |v| demodulify(v) }
      end
    end

    def build_plan
      @build_plan ||= nix_eval_json('.#confctl.buildPlan')
    end

    def inputs_info_installable(host)
      ".#confctl.inputsInfo.#{machine_key_for(host)}"
    end

    def inputs_installable(host)
      ".#confctl.inputs.#{machine_key_for(host)}"
    end

    def nix_eval_json(installable, impure: nil, settings: nil, apply: nil, configuration: true)
      ConfDir.require_flake!(conf_dir) if configuration
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
      ConfDir.require_flake!(conf_dir)
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

    def nix_build_output_path(installable)
      outputs = nix_build_json([installable])
      output_path = outputs.dig(0, 'outputs', 'out')

      unless output_path
        raise ConfCtl::Error, "missing output path for #{installable.inspect}"
      end

      output_path
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
