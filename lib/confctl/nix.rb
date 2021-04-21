require 'confctl/utils/file'
require 'etc'
require 'json'
require 'securerandom'
require 'tempfile'

module ConfCtl
  class Nix
    include Utils::File

    def initialize(conf_dir: nil, show_trace: false, max_jobs: nil)
      @conf_dir = conf_dir || ConfDir.path
      @show_trace = show_trace
      @max_jobs = max_jobs || Settings.instance.max_jobs
      @cmd = SystemCommand.new
    end

    def confctl_settings
      nix_instantiate({
        confDir: conf_dir,
        build: :confctl,
      })['confctl']
    end

    # Returns an array with deployment fqdns
    # @return [Array<String>]
    def list_deployment_fqdns
      nix_instantiate({
        confDir: conf_dir,
        build: :list,
      })['deployments']
    end

    # Return deployments and their config in a hash
    # @return [Hash]
    def list_deployments
      nix_instantiate({
        confDir: conf_dir,
        build: :info,
      }, core_swpins: true)
    end

    def list_swpins_channels
      nix_instantiate({
        confDir: conf_dir,
        build: :listSwpinsChannels,
      })
    end

    # Evaluate swpins for host
    # @return [Hash]
    def eval_core_swpins
      with_argument({
        confDir: conf_dir,
        build: :evalCoreSwpins,
      }) do |arg|
        out_link = File.join(
          cache_dir,
          'build',
          'core.swpins',
        )

        cmd_args = [
          'nix-build',
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix'),
        ].flatten.compact

        out, _ = cmd.run(*cmd_args).stdout

        JSON.parse(File.read(out.strip))
      end
    end

    # Evaluate swpins for host
    # @param host [String]
    # @return [Hash]
    def eval_host_swpins(host)
      with_argument({
        confDir: conf_dir,
        build: :evalHostSwpins,
        deployments: [host],
      }, core_swpins: true) do |arg|
        out_link = File.join(
          cache_dir,
          'build',
          "#{ConfCtl.safe_host_name(host)}.swpins",
        )

        cmd_args = [
          'nix-build',
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix'),
        ].flatten.compact

        out, _ = cmd.run(*cmd_args)

        JSON.parse(File.read(out.strip))[host]
      end
    end

    # Build config.system.build.toplevel for selected hosts
    #
    # @param hosts [Array<String>]
    # @param swpin_paths [Hash]
    # @param host_swpin_specs [Hash]
    # @param time [Time]
    #
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    #
    # @return [Hash<String, Generation::Build>]
    def build_toplevels(hosts: [], swpin_paths: {}, host_swpin_specs: {}, time: nil, &block)
      with_argument({
        confDir: conf_dir,
        build: :toplevel,
        deployments: hosts,
      }) do |arg|
        time ||= Time.now
        ret_generations = {}
        out_link = File.join(cache_dir, 'build', "#{SecureRandom.hex(4)}.build")

        cmd_args = [
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix'),
        ].flatten.compact

        nb = NixBuild.new(cmd_args, swpin_paths)
        nb.run(&block)

        begin
          host_toplevels = JSON.parse(File.read(out_link))
          host_toplevels.each do |host, toplevel|
            host_generations = Generation::BuildList.new(host)
            generation = host_generations.find(toplevel, swpin_paths)

            if generation.nil?
              generation = Generation::Build.new(host)
              generation.create(
                toplevel,
                swpin_paths,
                host_swpin_specs[host],
                date: time,
              )
              generation.save
            end

            host_generations.current = generation
            ret_generations[host] = generation
          end
        ensure
          unlink_if_exists(out_link)
        end

        ret_generations
      end
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    #
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    #
    # @return [Boolean]
    def copy(dep, toplevel, &block)
      if dep.localhost?
        true
      else
        cp = NixCopy.new(dep.target_host, toplevel)
        cp.run!(&block).success?
      end
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    # @param action [String]
    # @return [Boolean]
    def activate(dep, toplevel, action)
      args = [File.join(toplevel, 'bin/switch-to-configuration'), action]

      MachineControl.new(dep).execute!(*args).success?
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    # @return [Boolean]
    def set_profile(dep, toplevel)
      args = [
        'nix-env',
        '-p', '/nix/var/nix/profiles/system',
        '--set', toplevel,
      ]

      MachineControl.new(dep).execute!(*args).success?
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

      cmd.run!(*args, env: {'shellHook' => nil}).success?
    end

    protected
    attr_reader :conf_dir, :show_trace, :max_jobs, :cmd

    # @param hash [Hash]
    # @param core_swpins [Boolean]
    # @yieldparam file [String]
    def with_argument(hash, core_swpins: false)
      if core_swpins
        paths = Swpins::Core.get.pre_evaluated_store_paths
        hash[:coreSwpins] = paths if paths
      end

      f = Tempfile.new('confctl')
      f.puts(hash.to_json)
      f.close
      yield(f.path)
    ensure
      f.unlink
    end

    def nix_instantiate(hash, opts = {})
      with_argument(hash, opts) do |arg|
        cmd_args = [
          'nix-instantiate',
          '--eval',
          '--json',
          '--strict',
          '--read-write-mode',
          '--arg', 'jsonArg', arg,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix'),
        ].flatten.compact

        out, _ = cmd.run(*cmd_args).stdout

        demodulify(JSON.parse(out))
      end
    end

    def demodulify(value)
      if value.is_a?(Array)
        value.each { |item| demodulify(item) }
      elsif value.is_a?(Hash)
        value.delete('_module')
        value.each { |k, v| demodulify(v) }
      end
    end

    def cache_dir
      ConfDir.cache_dir
    end
  end
end
