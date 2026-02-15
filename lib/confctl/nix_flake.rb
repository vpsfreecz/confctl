require 'json'
require_relative 'nix_build_flake'

module ConfCtl
  class NixFlake < NixLegacy
    def confctl_settings
      @confctl_settings ||= begin
        settings = nix_eval_json('.#confctl.settings', impure: false)
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
      demodulify(machines)
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
        [host, host_plan['swpinPaths'] || {}]
      end
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
    def build_attributes(hosts: [], swpin_paths: {}, host_swpin_specs: {}, time: nil, &block)
      plan = build_plan
      time ||= Time.now
      ret_generations = {}
      host_plans = {}
      installables = []

      hosts.each do |host|
        host_plan = plan.fetch(host)
        flake_key = host_plan['flakeKey']
        host_plans[host] = host_plan
        installables << ".#confctl.build.#{flake_key}.toplevel"
        installables << ".#confctl.build.#{flake_key}.autoRollback"
      end

      build_results = nix_build_json(installables, &block)
      installable_paths = map_installables(installables, build_results)

      hosts.each do |host|
        host_plan = host_plans[host]
        flake_key = host_plan['flakeKey']
        host_swpin_paths = host_plan['swpinPaths'] || {}
        swpin_specs_json = host_plan['swpinSpecJson'] || {}

        specs = swpin_specs_json.to_h do |name, spec_json|
          spec_class = ConfCtl::Swpins::Spec.for(spec_json['type'].to_sym)
          spec = spec_class.new(
            spec_json['name'] || name,
            spec_json['nix_options'],
            spec_json
          )
          [name, spec]
        end

        toplevel_installable = ".#confctl.build.#{flake_key}.toplevel"
        rollback_installable = ".#confctl.build.#{flake_key}.autoRollback"
        toplevel_path = installable_paths[toplevel_installable]
        auto_rollback_path = installable_paths[rollback_installable]

        host_generations = Generation::BuildList.new(host)
        generation = host_generations.find(toplevel_path, host_swpin_paths)

        if generation.nil?
          generation = Generation::Build.new(host)
          generation.create(
            toplevel_path,
            auto_rollback_path,
            host_swpin_paths,
            specs,
            date: time
          )
          generation.save
        end

        host_generations.current = generation
        ret_generations[host] = generation
      end

      ret_generations
    end

    protected

    def build_plan
      @build_plan ||= nix_eval_json('.#confctl.buildPlan')
    end

    def nix_eval_json(installable, impure: nil)
      impure_flag = impure.nil? ? impure_eval? : impure

      result = run_nix_with_fallback do |extra_experimental, no_update_lock_file|
        nix_eval_args(
          installable,
          impure: impure_flag,
          extra_experimental: extra_experimental,
          no_update_lock_file: no_update_lock_file
        )
      end

      out, = result.stdout
      JSON.parse(out)
    end

    def nix_build_json(installables, &block)
      impure_flag = impure_eval?
      extra_experimental = false
      no_update_lock_file = true

      loop do
        args = nix_build_args(
          installables,
          impure: impure_flag,
          extra_experimental: extra_experimental,
          no_update_lock_file: no_update_lock_file
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

    def nix_eval_args(installable, impure:, extra_experimental:, no_update_lock_file:)
      args = ['nix', 'eval', '--json']
      args.concat(
        nix_common_args(
          impure: impure,
          extra_experimental: extra_experimental,
          no_update_lock_file: no_update_lock_file
        )
      )
      args << installable
      args
    end

    def nix_build_args(installables, impure:, extra_experimental:, no_update_lock_file:)
      args = ['--json', '--no-link']
      args.concat(
        nix_common_args(
          impure: impure,
          extra_experimental: extra_experimental,
          no_update_lock_file: no_update_lock_file
        )
      )
      args.concat(installables)
      args
    end

    def nix_common_args(impure:, extra_experimental:, no_update_lock_file:)
      args = []
      if extra_experimental
        args << '--extra-experimental-features' << 'nix-command'
        args << '--extra-experimental-features' << 'flakes'
      end

      args << '--no-write-lock-file'
      args << '--no-update-lock-file' if no_update_lock_file
      args << '--override-input' << 'confctl' << "path:#{ConfCtl.root}"

      args << '--show-trace' if show_trace

      if max_jobs
        args << '--option' << 'max-jobs' << max_jobs.to_s
      end

      if cores
        args << '--option' << 'cores' << cores.to_s
      end

      args << '--impure' if impure
      args
    end

    def map_installables(installables, build_results)
      installables.zip(build_results).to_h do |installable, result|
        outputs = result['outputs'] || {}
        path = outputs['out'] || outputs.values.first
        [installable, path]
      end
    end

    def no_update_lock_file_error?(message)
      message.match?(/--no-update-lock-file/) && message.match?(/unknown|unrecognized|invalid|unsupported/i)
    end

    def experimental_error?(message)
      message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
    end

    def impure_eval?
      settings = confctl_settings
      settings.fetch('nix', {}).fetch('impureEval', false) == true
    end
  end
end
