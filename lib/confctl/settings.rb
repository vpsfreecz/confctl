require 'singleton'

module ConfCtl
  class Settings
    include Singleton

    def initialize
      @settings = nil
    end

    def list_columns
      read_settings { |s| s['list']['columns'] }
    end

    def max_jobs
      read_settings { |s| s['nix']['maxJobs'] }
    end

    def nix_paths
      read_settings { |s| s['nix']['nixPath'] }
    end

    def core_swpin_channels
      read_settings { |s| s['swpins']['core']['channels'] }
    end

    def core_swpin_pins
      read_settings { |s| s['swpins']['core']['pins'] }
    end

    def build_generations
      read_settings { |s| s['buildGenerations'] }
    end

    def host_generations
      read_settings { |s| s['hostGenerations'] }
    end

    protected
    def read_settings
      if @settings.nil?
        nix = Nix.new(max_jobs: 'auto')
        @settings = nix.confctl_settings
      end

      yield(@settings)
    end
  end
end
