require 'fileutils'
require 'json'

module ConfCtl
  class Swpins::Core
    # @return [Swpins::Core]
    def self.get
      return @instance if @instance

      @instance = new
      @instance.parse
      @instance
    end

    include Utils::File

    # @return [String]
    attr_reader :name

    # @return [String]
    attr_reader :path

    # @return [Hash<String, Swpins::Specs::Base>]
    attr_reader :specs

    # @return [Array<Swpins::Channel>]
    attr_reader :channels

    def initialize
      @name = 'core'
      @path = File.join(ConfCtl::Swpins.core_dir, 'core.json')

      settings = ConfCtl::Settings.instance

      @channels = Swpins::ChannelList.get.select do |c|
        settings.core_swpin_channels.include?(c.name)
      end

      @nix_specs = settings.core_swpin_pins
    end

    def parse
      @specs = {}

      # Add specs from channels
      channels.each do |chan|
        chan.specs.each do |name, chan_spec|
          s = chan_spec.clone
          s.channel = chan.name
          specs[name] = s
        end
      end

      # Add core-specific specs
      @json_specs = if File.exist?(path)
                      JSON.parse(File.read(path))
                    else
                      {}
                    end

      nix_specs.each do |name, nix_opts|
        specs[name] = Swpins::Spec.for(nix_opts['type'].to_sym).new(
          name,
          nix_opts[nix_opts['type']],
          json_specs[name]
        )
      end
    end

    def valid?
      specs.values.all?(&:valid?)
    end

    def save
      custom = {}

      specs.each do |name, s|
        custom[name] = s unless s.from_channel?
      end

      if custom.empty?
        begin
          File.unlink(path)
        rescue Errno::ENOENT
        end
      else
        tmp = "#{path}.new"

        FileUtils.mkdir_p(ConfCtl::Swpins.core_dir)

        File.open(tmp, 'w') do |f|
          f.puts(JSON.pretty_generate(custom))
        end

        File.rename(tmp, path)
      end
    end

    def pre_evaluate
      nix = ConfCtl::Nix.new
      paths = nix.eval_core_swpins

      FileUtils.mkdir_p(cache_dir)

      paths.each do |pin, path|
        name = "core-swpin.#{pin}"
        link = File.join(cache_dir, name)
        replace_symlink(link, path)
        ConfCtl::GCRoot.add(name, link) unless ConfCtl::GCRoot.exist?(name)
      end
    end

    def pre_evaluated_store_paths
      path = File.join(cache_dir, 'core.swpins')
      return unless File.exist?(path)

      swpins = JSON.parse(File.read(path))

      swpins.each_value do |path|
        return unless Dir.exist?(path)
      end

      swpins
    end

    protected

    attr_reader :nix_specs, :json_specs

    def cache_dir
      File.join(ConfDir.cache_dir, 'build')
    end
  end
end
