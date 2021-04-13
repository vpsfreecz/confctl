require 'singleton'

module ConfCtl
  class Settings
    include Singleton

    def initialize
      nix = Nix.new
      @settings = nix.confctl_settings
    end

    def list_columns
      @settings['list']['columns']
    end

    def nix_paths
      @settings['nix']['nixPath']
    end

    def core_swpin_channels
      @settings['swpins']['core']['channels']
    end

    def core_swpin_pins
      @settings['swpins']['core']['pins']
    end

    def build_generations
      @settings['buildGenerations']
    end

    def host_generations
      @settings['hostGenerations']
    end
  end
end
