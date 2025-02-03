require 'confctl/utils/file'
require 'etc'
require 'json'
require 'securerandom'
require 'tempfile'

module ConfCtl
  class Nix
    # Create a new instance without access to {ConfCtl::Settings}, i.e. when
    # called outside of cluster configuration directory.
    # @return [Nix]
    def self.stateless(show_trace: false, max_jobs: 'auto')
      new(show_trace:, max_jobs:)
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
      out_link = File.join(
        cache_dir,
        'build',
        'settings.json'
      )

      with_cache(out_link) do
        with_argument({
          confDir: conf_dir,
          build: :confctl
        }) do |arg|
          cmd_args = [
            'nix-build',
            '--arg', 'jsonArg', arg,
            '--out-link', out_link,
            (show_trace ? '--show-trace' : nil),
            (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
            (cores ? ['--cores', cores.to_s] : nil),
            ConfCtl.nix_asset('evaluator.nix')
          ].flatten.compact

          cmd.run(*cmd_args)
        end
      end

      demodulify(JSON.parse(File.read(out_link))['confctl'])
    end

    # Returns an array with options from all confctl modules
    # @return [Array]
    def module_options
      options = nix_instantiate({
        confDir: conf_dir,
        build: :moduleOptions
      })
    end

    # Returns an array with machine fqdns
    # @return [Array<String>]
    def list_machine_fqdns
      nix_instantiate({
        confDir: conf_dir,
        build: :list
      })['machines']
    end

    # Return machines and their config in a hash
    # @return [Hash]
    def list_machines
      out_link = File.join(
        cache_dir,
        'build',
        'machine-list.json'
      )

      with_cache(out_link) do
        with_argument({
          confDir: conf_dir,
          build: :info
        }, core_swpins: true) do |arg|
          cmd_args = [
            'nix-build',
            '--arg', 'jsonArg', arg,
            '--out-link', out_link,
            (show_trace ? '--show-trace' : nil),
            (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
            (cores ? ['--cores', cores.to_s] : nil),
            ConfCtl.nix_asset('evaluator.nix')
          ].flatten.compact

          cmd.run(*cmd_args)
        end
      end

      demodulify(JSON.parse(File.read(out_link)))
    end

    def list_swpins_channels
      nix_instantiate({
        confDir: conf_dir,
        build: :listSwpinsChannels
      })
    end

    # Evaluate swpins for host
    # @return [Hash]
    def eval_core_swpins
      with_argument({
        confDir: conf_dir,
        build: :evalCoreSwpins
      }) do |arg|
        out_link = File.join(
          cache_dir,
          'build',
          'core.swpins'
        )

        cmd_args = [
          'nix-build',
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          (cores ? ['--cores', cores.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix')
        ].flatten.compact

        out, = cmd.run(*cmd_args).stdout

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
        machines: [host]
      }, core_swpins: true) do |arg|
        out_link = File.join(
          cache_dir,
          'build',
          "#{ConfCtl.safe_host_name(host)}.swpins"
        )

        cmd_args = [
          'nix-build',
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          (cores ? ['--cores', cores.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix')
        ].flatten.compact

        out, = cmd.run(*cmd_args)

        JSON.parse(File.read(out.strip))[host]
      end
    end

    # Build config.system.build.toplevel for selected hosts
    #
    # @param hosts [Array<Machine>]
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
      with_argument({
        confDir: conf_dir,
        build: :toplevel,
        machines: hosts
      }) do |arg|
        time ||= Time.now
        ret_generations = {}
        out_link = File.join(cache_dir, 'build', "#{SecureRandom.hex(4)}.build")

        cmd_args = [
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          (cores ? ['--cores', cores.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix')
        ].flatten.compact

        nb = NixBuild.new(cmd_args, swpin_paths)
        nb.run(&block)

        begin
          host_results = JSON.parse(File.read(out_link))
          host_results.each do |host, result|
            host_generations = Generation::BuildList.new(host)
            generation = host_generations.find(result['attribute'], swpin_paths)

            if generation.nil?
              generation = Generation::Build.new(host)
              generation.create(
                result['attribute'],
                result['autoRollback'],
                swpin_paths,
                host_swpin_specs[host],
                date: time
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

    # @param machine [Machine]
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
        cp = NixCopy.new(machine.carrier_machine.target_host, paths)
        cp.run!(&).success?
      else
        cp = NixCopy.new(machine.target_host, paths)
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

      loop do
        out, = MachineControl.new(machine, logger:).execute!('cat', check_file, '2>/dev/null')
        stripped = out.strip
        break if stripped == 'switched' || ((t + timeout + 10) < Time.now && stripped != 'switching')

        sleep(1)
      end

      # Confirm it
      10.times do
        break if MachineControl.new(machine, logger:).execute!('sh', '-c', "'echo confirmed > #{check_file}'").success?
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

    # Execute block only if `out_link` does not exist or conf dir has changed
    # @param out_link [String] out link path
    def with_cache(out_link)
      unchanged = false

      if File.exist?(out_link)
        unchanged = ConfDir.unchanged?

        if unchanged
          cache_mtime = ConfDir.state_mtime

          if cache_mtime
            begin
              st = File.lstat(out_link)
            rescue Errno::ENOENT
              # pass
            else
              if st.mtime > cache_mtime
                Logger.instance << "Using #{out_link}\n"
                return
              end
            end
          end
        end
      end

      Logger.instance << "Building #{out_link}\n"

      unless unchanged
        Logger.instance << "Updating configuration cache\n"
        ConfDir.update_state
      end
      yield
    end

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

    def nix_instantiate(hash, **opts)
      with_argument(hash, **opts) do |arg|
        cmd_args = [
          'nix-instantiate',
          '--eval',
          '--json',
          '--strict',
          '--read-write-mode',
          '--arg', 'jsonArg', arg,
          (show_trace ? '--show-trace' : nil),
          (max_jobs ? ['--max-jobs', max_jobs.to_s] : nil),
          (cores ? ['--cores', cores.to_s] : nil),
          ConfCtl.nix_asset('evaluator.nix')
        ].flatten.compact

        out, = cmd.run(*cmd_args).stdout

        demodulify(JSON.parse(out))
      end
    end

    def demodulify(value)
      if value.is_a?(Array)
        value.each { |item| demodulify(item) }
      elsif value.is_a?(Hash)
        value.delete('_module')
        value.each_value { |v| demodulify(v) }
      end
    end

    def cache_dir
      ConfDir.cache_dir
    end
  end
end
