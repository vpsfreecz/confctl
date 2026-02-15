module ConfCtl
  module Nix
    class Args
      DEFAULT_LEGACY_NAMES = %w[nixpkgs vpsadminos vpsadmin].freeze

      def initialize(settings:, impure: nil, no_update_lock_file: true)
        @settings = settings || {}
        @impure_override = impure
        @no_update_lock_file = no_update_lock_file
      end

      def impure?
        return @impure_override unless @impure_override.nil?

        @settings.dig('nix', 'impureEval') == true
      end

      def legacy_nix_path?
        @settings.dig('nix', 'legacyNixPath') == true
      end

      def legacy_names
        names = @settings.dig('nix', 'legacyNixPathMap')
        return DEFAULT_LEGACY_NAMES if names.nil?

        names
      end

      def eval_args
        base_args
      end

      def build_args
        base_args
      end

      private

      def base_args
        args = []
        args << '--impure' if impure?
        args << '--no-write-lock-file'
        args << '--no-update-lock-file' if @no_update_lock_file
        args
      end
    end
  end
end
