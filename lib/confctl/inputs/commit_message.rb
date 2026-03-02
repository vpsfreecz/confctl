require 'confctl/git_repo_mirror'

module ConfCtl
  module Inputs
    class CommitMessage
      def self.build(changes:, changelog:, downgrade: false, action: :update)
        raise ArgumentError, 'no changes' if changes.nil? || changes.empty?

        title = build_title(changes, action)

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

      def self.build_title(changes, action)
        names = changes.map(&:name).sort
        verb = action == :set ? 'set' : 'update'
        title = "inputs: #{verb} #{names.join(', ')}"
        short_rev = common_short_rev(changes)

        short_rev ? "#{title} to #{short_rev}" : title
      end

      def self.common_short_rev(changes)
        reference = changes.first

        return nil if reference.new_rev.nil? || reference.new_short_rev.nil?
        return nil if changes.any? { |ch| ch.new_rev != reference.new_rev }
        return reference.new_short_rev if changes.length == 1
        return nil unless same_sources?(changes, reference.url)

        reference.new_short_rev
      end

      def self.same_sources?(changes, reference_url)
        normalized_reference = normalize_url(reference_url)
        return false if normalized_reference.nil?

        changes.all? { |ch| normalize_url(ch.url) == normalized_reference }
      end

      def self.normalize_url(url)
        return nil if url.nil?

        url.sub(%r{/+\z}, '')
      end

      private_class_method :build_title, :common_short_rev, :same_sources?, :normalize_url
    end
  end
end
