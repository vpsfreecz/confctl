require 'json'
require 'net/http'
require 'uri'

module ConfCtl
  module Inputs
    class GitHubCompare
      class Error < StandardError; end

      SHA = /\A[0-9a-f]{40}\z/
      NAME = /\A[A-Za-z0-9_.-]+\z/

      def initialize(token: ENV['GITHUB_TOKEN'] || ENV.fetch('GH_TOKEN', nil), http: Net::HTTP)
        @token = token
        @http = http
      end

      # Returns :ahead, :behind or :diverged; unavailable/inconsistent evidence
      # raises Error. The paginated commits list is deliberately not authority.
      def compare(owner:, repo:, old_rev:, new_rev:)
        unless [owner, repo].all? { |v| v.is_a?(String) && NAME.match?(v) && !%w[. ..].include?(v) } &&
               [old_rev, new_rev].all? { |v| v.is_a?(String) && SHA.match?(v) }
          raise Error, 'GitHub comparison requires a repository and full commit SHAs'
        end
        return :identical if old_rev == new_rev

        uri = URI("https://api.github.com/repos/#{owner}/#{repo}/compare/#{old_rev}...#{new_rev}?per_page=1")
        request = Net::HTTP::Get.new(uri)
        request['Accept'] = 'application/vnd.github+json'
        request['X-GitHub-Api-Version'] = '2022-11-28'
        request['User-Agent'] = 'confctl-forward-guard'
        request['Authorization'] = "Bearer #{@token}" if @token && !@token.empty?

        # No redirects: a token must never follow a Location to another host.
        response = @http.start(uri.host, uri.port, use_ssl: true, open_timeout: 10, read_timeout: 30) do |http|
          http.max_retries = 0
          http.request(request)
        end
        raise Error, "GitHub comparison failed (HTTP #{response.code})" unless response.code == '200'

        summary(JSON.parse(response.body), old_rev, new_rev)
      rescue JSON::ParserError
        raise Error, 'Malformed GitHub comparison response'
      rescue Timeout::Error, SocketError, SystemCallError, IOError, OpenSSL::SSL::SSLError, Net::HTTPBadResponse
        # Network exceptions can contain request data; never forward their text.
        raise Error, 'Unable to obtain GitHub ancestry evidence'
      end

      protected

      def summary(data, old_rev, new_rev)
        unless data.is_a?(Hash) && data['base_commit'].is_a?(Hash) &&
               data['base_commit']['sha'] == old_rev && data['merge_base_commit'].is_a?(Hash) &&
               SHA.match?(data['merge_base_commit']['sha'].to_s) &&
               %w[ahead_by behind_by total_commits].all? { |key| data[key].is_a?(Integer) && data[key] >= 0 }
          raise Error, 'Incomplete GitHub ancestry evidence'
        end

        ahead = data['ahead_by']
        behind = data['behind_by']
        base = data['merge_base_commit']['sha']
        case data['status']
        when 'ahead'
          return :ahead if ahead > 0 && behind == 0 && base == old_rev && data['total_commits'] == ahead
        when 'behind'
          return :behind if ahead == 0 && behind > 0 && base == new_rev && data['total_commits'] == 0
        when 'diverged'
          return :diverged if ahead > 0 && behind > 0 && ![old_rev, new_rev].include?(base) &&
                              data['total_commits'] == ahead
        end

        raise Error, 'Contradictory GitHub ancestry evidence'
      end
    end
  end
end
