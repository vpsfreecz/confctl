require 'fileutils'
require 'json'
require 'time'

module ConfCtl
  class Generation::Build
    class UnsupportedFormat < Error; end
    class InvalidGeneration < Error; end

    # @return [String]
    attr_reader :host

    # @return [String]
    attr_reader :mode

    # @return [String]
    attr_reader :name

    # @return [Time]
    attr_reader :date

    # @return [String]
    attr_reader :toplevel

    # @return [String]
    attr_reader :auto_rollback

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
    # @param inputs [Hash]
    # @param inputs_info [Hash]
    # @param date [Time]
    def create_flake(toplevel, auto_rollback, inputs:, inputs_info:, date: nil)
      @mode = 'flakes'
      @toplevel = toplevel
      @auto_rollback = auto_rollback
      @inputs = inputs
      @inputs_info = inputs_info
      @date = date || Time.now
      @name = @date.strftime('%Y-%m-%d--%H-%M-%S')
      @kernel_version = extract_kernel_version
    end

    # @param name [String]
    def load(name)
      @name = name

      cfg = JSON.parse(File.read(config_path))
      unless cfg.is_a?(Hash)
        raise InvalidGeneration, "#{config_path}: expected a JSON object"
      end
      unless cfg['mode'] == 'flakes'
        raise UnsupportedFormat, "#{config_path}: unsupported generation mode #{cfg['mode'].inspect}; only explicit flakes mode is supported"
      end

      @mode = 'flakes'
      @toplevel = cfg.fetch('toplevel')
      @auto_rollback = cfg['auto_rollback']
      @inputs = cfg.fetch('inputs', {})
      @inputs_info = cfg['inputs_info'] || cfg['inputsInfo'] || {}
      unless toplevel.is_a?(String) && !toplevel.empty? &&
             (auto_rollback.nil? || auto_rollback.is_a?(String)) &&
             inputs.is_a?(Hash) && inputs.all? { |role, path| role.is_a?(String) && path.is_a?(String) } &&
             inputs_info.is_a?(Hash)
        raise InvalidGeneration, "#{config_path}: invalid flake generation payload"
      end

      @date = Time.iso8601(cfg['date'])
      @kernel_version = extract_kernel_version
    rescue UnsupportedFormat, InvalidGeneration
      raise
    rescue StandardError => e
      raise InvalidGeneration, "#{config_path}: #{e.message}"
    end

    def save
      FileUtils.mkdir_p(dir)
      File.symlink(toplevel, toplevel_path)
      File.symlink(auto_rollback, auto_rollback_path)

      inputs.each do |role, path|
        File.symlink(path, input_path(role))
      end

      File.open(config_path, 'w') do |f|
        f.puts(JSON.pretty_generate({
          mode: 'flakes',
          date: date.iso8601,
          toplevel:,
          auto_rollback:,
          inputs:,
          inputs_info:
        }))
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

      inputs.each_key do |role|
        path = input_path(role)
        File.unlink(path) if File.exist?(path) || File.symlink?(path)
      end

      File.unlink(config_path)
      Dir.rmdir(dir)
    end

    def add_gcroot
      GCRoot.add(gcroot_name('toplevel'), toplevel_path)
      GCRoot.add(gcroot_name('auto_rollback'), auto_rollback_path)
      inputs.each_key do |role|
        GCRoot.add(gcroot_name("input.#{role}"), input_path(role))
      end
    end

    def remove_gcroot
      GCRoot.remove(gcroot_name('toplevel'))
      GCRoot.remove(gcroot_name('auto_rollback'))
      inputs.each_key do |role|
        GCRoot.remove(gcroot_name("input.#{role}"))
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

    def input_path(role)
      File.join(dir, "#{role}.input")
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
