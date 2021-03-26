require 'confctl/utils/file'
require 'etc'
require 'json'
require 'securerandom'
require 'tempfile'

module ConfCtl
  class Nix
    include Utils::File

    def initialize(conf_dir: nil, show_trace: false)
      @conf_dir = conf_dir || ConfCtl.conf_dir
      @show_trace = show_trace
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
      })
    end

    def list_swpins_channels
      nix_instantiate({
        confDir: conf_dir,
        build: :listSwpinsChannels,
      })
    end

    # Evaluate swpins for host
    # @param host [String]
    # @return [Hash]
    def eval_swpins(host)
      with_argument({
        confDir: conf_dir,
        build: :evalSwpins,
        deployments: [host],
      }) do |arg|
        out_link = File.join(
          cache_dir,
          'build',
          "#{ConfCtl.safe_host_name(host)}.swpins",
        )

        cmd = [
          'nix-build',
          '--arg', 'jsonArg', arg,
          '--out-link', out_link,
          (show_trace ? '--show-trace' : ''),
          ConfCtl.nix_asset('evaluator.nix'),
        ]

        output = `#{cmd.join(' ')}`.strip

        if $?.exitstatus != 0
          fail "nix-build failed with exit status #{$?.exitstatus}"
        end

        JSON.parse(File.read(output))[host]
      end
    end

    # Build config.system.build.toplevel for selected hosts
    # @param hosts [Array<String>]
    # @param swpin_paths [Hash]
    # @param host_swpin_specs [Hash]
    # @param time [Time]
    # @return [Hash<String, BuildGeneration>]
    def build_toplevels(hosts: [], swpin_paths: {}, host_swpin_specs: {}, time: nil)
      with_argument({
        confDir: conf_dir,
        build: :toplevel,
        deployments: hosts,
      }) do |arg|
        time ||= Time.now
        ret_generations = {}
        out_link = File.join(cache_dir, 'build', "#{SecureRandom.hex(4)}.build")

        pid = Process.fork do
          ENV['NIX_PATH'] = build_nix_path(swpin_paths)

          Process.exec(*[
            'nix-build',
            '--arg', 'jsonArg', arg,
            '--out-link', out_link,
            (show_trace ? '--show-trace' : nil),
            ConfCtl.nix_asset('evaluator.nix'),
          ].compact)
        end

        Process.wait(pid)

        if $?.exitstatus != 0
          fail "nix-build failed with exit status #{$?.exitstatus}"
        end

        begin
          host_toplevels = JSON.parse(File.read(out_link))
          host_toplevels.each do |host, toplevel|
            host_generations = BuildGenerationList.new(host)
            generation = host_generations.find(toplevel, swpin_paths)

            if generation.nil?
              generation = BuildGeneration.new(host)
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
    # @return [Boolean]
    def copy(dep, toplevel)
      if dep.localhost?
        true
      else
        system('nix', 'copy', '--to', "ssh://root@#{dep.target_host}", toplevel)
      end
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    # @param action [String]
    # @return [Boolean]
    def activate(dep, toplevel, action)
      args = [File.join(toplevel, 'bin/switch-to-configuration'), action]

      MachineControl.new(dep).execute(*args).success?
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

      MachineControl.new(dep).execute(*args).success?
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

      pid = Process.fork do
        ENV.delete('shellHook')
        Process.exec(*args)
      end

      Process.wait(pid)
      $?.exitstatus == 0
    end

    protected
    attr_reader :conf_dir, :show_trace

    # @param hash [Hash]
    # @yieldparam file [String]
    def with_argument(hash)
      f = Tempfile.new('confctl')
      f.puts(hash.to_json)
      f.close
      yield(f.path)
    ensure
      f.unlink
    end

    def nix_instantiate(hash)
      with_argument(hash) do |arg|
        cmd = [
          'nix-instantiate',
          '--eval',
          '--json',
          '--strict',
          '--read-write-mode',
          '--arg', 'jsonArg', arg,
          (show_trace ? '--show-trace' : ''),
          ConfCtl.nix_asset('evaluator.nix'),
        ]

        json = `#{cmd.join(' ')}`

        if $?.exitstatus != 0
          fail "nix-instantiate failed with exit status #{$?.exitstatus}"
        end

        demodulify(JSON.parse(json))
      end
    end

    def build_nix_path(swpins)
      paths = []
      paths << "confctl=#{ConfCtl.root}"
      paths.concat(swpins.map { |k, v| "#{k}=#{v}" })
      paths.concat(Settings.instance.nix_paths)
      paths.join(':')
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
      ConfCtl.cache_dir
    end
  end
end
