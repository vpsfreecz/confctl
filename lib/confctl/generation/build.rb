require 'fileutils'
require 'json'
require 'time'

module ConfCtl
  class Generation::Build
    # @return [String]
    attr_reader :host

    # @return [String]
    attr_reader :name

    # @return [Time]
    attr_reader :date

    # @return [String]
    attr_reader :toplevel

    # @return [String]
    attr_reader :auto_rollback

    # @return [Array<String>]
    attr_reader :swpin_names

    # @return [Hash]
    attr_reader :swpin_paths

    # @return [Hash]
    attr_reader :swpin_specs

    # @return [Hash, nil]
    attr_reader :inputs_info

    # @return [Hash, nil]
    attr_reader :inputs

    # @param current [Boolean]
    # @return [Boolean]
    attr_accessor :current

    # @return [String, nil]
    attr_reader :kernel_version

    # @param host [String]
    def initialize(host)
      @host = host
    end

    # @param toplevel [String]
    # @param auto_rollback [String]
    # @param swpin_paths [Hash]
    # @param swpin_specs [Hash]
    # @param date [Time]
    # @param inputs_info [Hash, nil]
    # @param inputs [Hash, nil]
    def create(toplevel, auto_rollback, swpin_paths, swpin_specs, date: nil, inputs_info: nil, inputs: nil)
      @toplevel = toplevel
      @auto_rollback = auto_rollback
      @swpin_names = swpin_paths.keys
      @swpin_paths = swpin_paths
      @swpin_specs = swpin_specs
      @inputs_info = inputs_info
      @inputs = inputs
      @date = date || Time.now
      @name = date.strftime('%Y-%m-%d--%H-%M-%S')
      @kernel_version = extract_kernel_version
    end

    # @param name [String]
    def load(name)
      @name = name

      cfg = JSON.parse(File.read(config_path))
      @toplevel = cfg['toplevel']
      @auto_rollback = cfg['auto_rollback']

      @swpin_names = []
      @swpin_paths = {}
      @swpin_specs = {}

      cfg['swpins'].each do |swpin_name, swpin|
        @swpin_names << swpin_name
        @swpin_paths[swpin_name] = swpin['path']
        @swpin_specs[swpin_name] = Swpins::Spec.for(swpin['spec']['type'].to_sym).new(
          swpin_name,
          swpin['spec']['nix_options'],
          swpin['spec']
        )
      end

      @inputs_info = cfg['inputs_info']
      @inputs = cfg['inputs']

      @date = Time.iso8601(cfg['date'])
      @kernel_version = extract_kernel_version
    rescue StandardError => e
      raise Error, "invalid generation '#{name}': #{e.message}"
    end

    def save
      FileUtils.mkdir_p(dir)
      File.symlink(toplevel, toplevel_path)
      File.symlink(auto_rollback, auto_rollback_path)

      swpin_paths.each do |name, path|
        File.symlink(path, swpin_path(name))
      end

      File.open(config_path, 'w') do |f|
        data = {
          date: date.iso8601,
          toplevel:,
          auto_rollback:,
          swpins: swpin_paths.to_h do |name, path|
            [name, { path:, spec: swpin_specs[name].as_json }]
          end
        }

        data[:inputs_info] = inputs_info if inputs_info
        data[:inputs] = inputs if inputs

        f.puts(JSON.pretty_generate(data))
      end

      add_gcroot
    end

    def destroy
      remove_gcroot
      File.unlink(toplevel_path)

      begin
        File.unlink(auto_rollback_path)
      rescue Errno::ENOENT
        # Older generations might not have auto_rollback
      end

      swpin_paths.each_key { |name| File.unlink(swpin_path(name)) }
      File.unlink(config_path)
      Dir.rmdir(dir)
    end

    def add_gcroot
      GCRoot.add(gcroot_name('toplevel'), toplevel_path)
      GCRoot.add(gcroot_name('auto_rollback'), auto_rollback_path)
      swpin_paths.each_key do |name|
        GCRoot.add(gcroot_name("swpin.#{name}"), toplevel_path)
      end
    end

    def remove_gcroot
      GCRoot.remove(gcroot_name('toplevel'))
      GCRoot.remove(gcroot_name('auto_rollback'))
      swpin_paths.each_key do |name|
        GCRoot.remove(gcroot_name("swpin.#{name}"))
      end
    end

    def dir
      @dir ||= File.join(ConfDir.generation_dir, escaped_host, name)
    end

    protected

    def config_path
      @config_path ||= File.join(dir, 'generation.json')
    end

    def toplevel_path
      @toplevel_path ||= File.join(dir, 'toplevel')
    end

    def auto_rollback_path
      @auto_rollback_path ||= File.join(dir, 'auto_rollback')
    end

    def swpin_path(name)
      File.join(dir, "#{name}.swpin")
    end

    def escaped_host
      @escaped_host ||= ConfCtl.safe_host_name(host)
    end

    def gcroot_name(file)
      "#{escaped_host}-generation-#{name}-#{file}"
    end

    def extract_kernel_version
      # `kernel` is for NixOS/vpsAdminOS and also carried NixOS machines (netboot)
      # `bzImage` is for carried vpsAdminOS machines (netboot)
      %w[kernel bzImage].each do |v|
        link = File.readlink(File.join(toplevel, v))
        next unless %r{\A/nix/store/[^-]+-linux-([^/]+)} =~ link

        return ::Regexp.last_match(1)
      rescue Errno::ENOENT, Errno::EINVAL
        next
      end

      nil
    end
  end
end
