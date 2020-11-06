require 'etc'
require 'json'
require 'securerandom'
require 'tempfile'

module ConfCtl
  class Nix
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
        out_link = File.join(cache_dir, 'gcroots', "#{escape_name(host)}.swpins")

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

        add_gcroot(
          "confctl-#{escape_name(host)}.swpins",
          File.absolute_path(out_link)
        )
        JSON.parse(File.read(output))[host]
      end
    end

    # Build config.system.build.toplevel for selected hosts
    # @param hosts [Array<String>]
    # @param swpins [Hash]
    # @return [Hash]
    def build_toplevels(hosts, swpins)
      with_argument({
        confDir: conf_dir,
        build: :toplevel,
        deployments: hosts,
      }) do |arg|
        gcroot = File.join(cache_dir, 'gcroots', "#{SecureRandom.hex(4)}.build")

        pid = Process.fork do
          ENV['NIX_PATH'] = build_nix_path(swpins)

          Process.exec(*[
            'nix-build',
            '--arg', 'jsonArg', arg,
            '--out-link', gcroot,
            (show_trace ? '--show-trace' : nil),
            ConfCtl.nix_asset('evaluator.nix'),
          ].compact)
        end

        Process.wait(pid)

        if $?.exitstatus != 0
          fail "nix-build failed with exit status #{$?.exitstatus}"
        end

        begin
          host_toplevels = JSON.parse(File.read(gcroot))
          host_toplevels.each do |host, toplevel|
            host_gcroot = File.join(
              cache_dir,
              'gcroots',
              "#{escape_name(host)}.toplevel"
            )
            replace_symlink(host_gcroot, toplevel)
            add_gcroot(
              "confctl-#{escape_name(host)}.toplevel",
              File.absolute_path(host_gcroot)
            )
          end
        ensure
          unlink_if_exists(gcroot)
        end
        host_toplevels
      end
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    # @return [Boolean]
    def copy(dep, toplevel)
      system('nix', 'copy', '--to', "ssh://root@#{dep.target_host}", toplevel)
    end

    # @param dep [Deployments::Deployment]
    # @param toplevel [String]
    # @param action [String]
    # @return [Boolean]
    def activate(dep, toplevel, action)
      system(
        'ssh', "root@#{dep.target_host}",
        File.join(toplevel, 'bin/switch-to-configuration'), action
      )
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

    def add_gcroot(name, path)
      replace_symlink(
        File.join('/nix/var/nix/gcroots/per-user', Etc.getlogin, name),
        path
      )
    end

    # Atomically replace or create symlink
    # @param path [String] symlink path
    # @param dst [String] destination
    def replace_symlink(path, dst)
      replacement = "#{path}.new-#{SecureRandom.hex(3)}"
      File.symlink(dst, replacement)
      File.rename(replacement, path)
    end

    def unlink_if_exists(path)
      File.unlink(path)
      true
    rescue Errno::ENOENT
      false
    end

    def escape_name(host)
      host.gsub(/\//, ':')
    end

    def cache_dir
      ConfCtl.cache_dir
    end
  end
end
