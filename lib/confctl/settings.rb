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

    def build_generations
      read_settings { |s| s['buildGenerations'] }
    end

    def host_generations
      read_settings { |s| s['hostGenerations'] }
    end

    protected

    def read_settings
      if @settings.nil?
        nix = Nix.stateless
        @settings = nix.confctl_settings
      end

      yield(@settings)
    end
  end
end
