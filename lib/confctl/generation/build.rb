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

    # @return [Array<String>]
    attr_reader :swpin_names

    # @return [Hash]
    attr_reader :swpin_paths

    # @return [Hash]
    attr_reader :swpin_specs

    # @param current [Boolean]
    # @return [Boolean]
    attr_accessor :current

    # @param host [String]
    def initialize(host)
      @host = host
    end

    # @param toplevel [String]
    # @param swpin_paths [Hash]
    # @param swpin_specs [Hash]
    # @param date [Time]
    def create(toplevel, swpin_paths, swpin_specs, date: nil)
      @toplevel = toplevel
      @swpin_names = swpin_paths.keys
      @swpin_paths = swpin_paths
      @swpin_specs = swpin_specs
      @date = date || Time.now
      @name = date.strftime('%Y-%m-%d--%H-%M-%S')
    end

    # @param name [String]
    def load(name)
      @name = name

      cfg = JSON.parse(File.read(config_path))
      @toplevel = cfg['toplevel']

      @swpin_names = []
      @swpin_paths = {}
      @swpin_specs = {}

      cfg['swpins'].each do |name, swpin|
        @swpin_names << name
        @swpin_paths[name] = swpin['path']
        @swpin_specs[name] = Swpins::Spec.for(swpin['spec']['type'].to_sym).new(
          name,
          swpin['spec']['nix_options'],
          swpin['spec'],
        )
      end

      @date = Time.iso8601(cfg['date'])

    rescue => e
      raise Error, "invalid generation '#{name}': #{e.message}"
    end

    def save
      FileUtils.mkdir_p(dir)
      File.symlink(toplevel, toplevel_path)

      swpin_paths.each do |name, path|
        File.symlink(path, swpin_path(name))
      end

      File.open(config_path, 'w') do |f|
        f.puts(JSON.pretty_generate({
          date: date.iso8601,
          toplevel: toplevel,
          swpins: Hash[swpin_paths.map do |name, path|
            [name, {path: path, spec: swpin_specs[name].as_json}]
          end],
        }))
      end

      add_gcroot
    end

    def destroy
      remove_gcroot
      File.unlink(toplevel_path)
      swpin_paths.each_key { |name| File.unlink(swpin_path(name)) }
      File.unlink(config_path)
      Dir.rmdir(dir)
    end

    def add_gcroot
      GCRoot.add(gcroot_name('toplevel'), toplevel_path)
      swpin_paths.each do |name, path|
        GCRoot.add(gcroot_name("swpin.#{name}"), toplevel_path)
      end
    end

    def remove_gcroot
      GCRoot.remove(gcroot_name('toplevel'))
      swpin_paths.each do |name, path|
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

    def swpin_path(name)
      File.join(dir, "#{name}.swpin")
    end

    def escaped_host
      @escaped_host ||= ConfCtl.safe_host_name(host)
    end

    def gcroot_name(file)
      "#{escaped_host}-generation-#{name}-#{file}"
    end
  end
end
