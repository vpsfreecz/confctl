require 'confctl/git_repo_mirror'

module ConfCtl
  module Inputs
    class GitCompare
      class Error < StandardError; end

      SHA = GitRepoMirror::SHA
      NAME = /\A[A-Za-z0-9_.-]+\z/

      def compare(owner:, repo:, old_rev:, new_rev:)
        unless [owner, repo].all? { |value| value.is_a?(String) && NAME.match?(value) && !%w[. ..].include?(value) } &&
               [old_rev, new_rev].all? { |value| value.is_a?(String) && SHA.match?(value) }
          raise Error, 'Git comparison requires a repository and full commit SHAs'
        end
        return :identical if old_rev == new_rev

        # Preserve the locked spelling and exact URL used by FlakeLock and
        # changelogs, so existing full mirrors share their objects and lock.
        GitRepoMirror.new("https://github.com/#{owner}/#{repo}", quiet: true).compare(old_rev, new_rev)
      rescue GitRepoMirror::Error => e
        raise Error, e.message
      end
    end
  end
end
