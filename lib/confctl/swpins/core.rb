require 'fileutils'
require 'json'

module ConfCtl
  class Swpins::Core
    # @return [String]
    attr_reader :name

    # @return [String]
    attr_reader :path

    # @return [Hash<String, Swpins::Specs::Base>]
    attr_reader :specs

    # @return [Array<Swpins::Channel>]
    attr_reader :channels

    # @param channels [Swpins::ChannelList]
    def initialize(channels)
      @name = 'core'
      @path = File.join(ConfCtl::Swpins.core_dir, 'core.json')

      settings = ConfCtl::Settings.instance

      @channels = channels.select do |c|
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
      if File.exist?(path)
        @json_specs = JSON.parse(File.read(path))
      else
        @json_specs = {}
      end

      nix_specs.each do |name, nix_opts|
        specs[name] = Swpins::Spec.for(nix_opts['type'].to_sym).new(
          name,
          nix_opts[nix_opts['type']],
          json_specs[name],
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

    protected
    attr_reader :nix_specs, :json_specs
  end
end
