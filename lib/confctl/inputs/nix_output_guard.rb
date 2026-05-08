module ConfCtl
  module Inputs
    module NixOutputGuard
      GITHUB_API_CACHE_FALLBACK =
        %r{unable to download 'https://api\.github\.com/.*using cached version}m
      GITHUB_RATE_LIMIT = /rate limit exceeded/i

      def self.check!(result)
        output = [result&.stdout, result&.stderr].compact.join("\n")
        return unless github_api_cache_fallback?(output)

        if github_rate_limit?(output)
          raise ConfCtl::Error, <<~MSG.strip
            Nix could not refresh GitHub input metadata because the GitHub API rate limit was exceeded.
            It used a cached version instead, so the selected input may not be the latest revision.
            Configure a GitHub token in Nix access-tokens or retry later.
          MSG
        end

        raise ConfCtl::Error, <<~MSG.strip
          Nix could not refresh GitHub input metadata and used a cached version instead.
          The selected input may not be the latest revision; retry after the GitHub API is reachable.
        MSG
      end

      def self.github_api_cache_fallback?(output)
        output.match?(GITHUB_API_CACHE_FALLBACK)
      end

      def self.github_rate_limit?(output)
        output.match?(GITHUB_RATE_LIMIT)
      end

      private_class_method :github_api_cache_fallback?, :github_rate_limit?
    end
  end
end
