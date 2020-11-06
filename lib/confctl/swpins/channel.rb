require 'fileutils'
require 'json'

module ConfCtl
  class Swpins::Channel
    # @return [String]
    attr_reader :path

    # @return [String]
    attr_reader :name

    # @return [Hash<String, Swpins::Specs::Base>]
    attr_reader :specs

    # @param channel_dir [String]
    # @param name [String]
    # @param nix_specs [Hash]
    def initialize(channel_dir, name, nix_specs)
      @channel_dir = channel_dir
      @name = name
      @path = File.join(channel_dir, "#{name}.json")
      @nix_specs = nix_specs
    end

    def parse
      if File.exist?(path)
        @json_specs = JSON.parse(File.read(path))
      else
        @json_specs = {}
      end

      @specs = Hash[nix_specs.map do |name, nix_opts|
        [
          name,
          Swpins::Spec.for(nix_opts['type'].to_sym).new(
            name,
            nix_opts[nix_opts['type']],
            json_specs[name],
          ),
        ]
      end]
    end

    def valid?
      specs.values.all?(&:valid?)
    end

    def save
      tmp = "#{path}.new"

      FileUtils.mkdir_p(channel_dir)

      File.open(tmp, 'w') do |f|
        f.puts(JSON.pretty_generate(specs))
      end

      File.rename(tmp, path)
    end

    protected
    attr_reader :channel_dir, :nix_specs, :json_specs
  end
end
