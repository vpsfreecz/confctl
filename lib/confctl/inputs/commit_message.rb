require 'confctl/git_repo_mirror'

module ConfCtl
  module Inputs
    class CommitMessage
      def self.build(changes:, changelog:, downgrade: false, action: :update)
        raise ArgumentError, 'no changes' if changes.nil? || changes.empty?

        names = changes.map(&:name).sort
        verb = action == :set ? 'set' : 'update'
        title = "inputs: #{verb} #{names.join(', ')}"

        body = +''

        changes.each do |ch|
          body << "#{ch.name}: #{ch.old_rev ? ch.old_short_rev : 'set'} -> #{ch.new_short_rev || '-'}\n"

          next unless changelog
          next unless ch.url && ch.old_rev && ch.new_rev
          next if ch.old_rev == ch.new_rev

          from = downgrade ? ch.new_rev : ch.old_rev
          to = downgrade ? ch.old_rev : ch.new_rev

          begin
            mirror = ConfCtl::GitRepoMirror.new(ch.url, quiet: true)
            mirror.setup
            body << mirror.log(from, to, opts: ['--oneline'])
            body << "\n"
          rescue StandardError => e
            body << "(changelog unavailable: #{e})\n\n"
          end
        end

        "#{title}\n\n#{body}".strip
      end
    end
  end
end
