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
  end
end
