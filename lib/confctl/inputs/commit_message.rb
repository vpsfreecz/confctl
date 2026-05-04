require 'confctl/git_repo_mirror'

module ConfCtl
  module Inputs
    class CommitMessage
      def self.build(changes:, changelog:, downgrade: false, action: :update)
        raise ArgumentError, 'no changes' if changes.nil? || changes.empty?

        title = build_title(changes, action)

        body =
          if changelog
            build_changelog_body(changes, downgrade)
          else
            build_plain_body(changes)
          end

        "#{title}\n\n#{body}".strip
      end

      def self.build_plain_body(changes)
        changes.each_with_object(+'') do |ch, body|
          body << change_summary(ch)
        end
      end

      def self.build_changelog_body(changes, downgrade)
        group_changes(changes).map do |group|
          build_changelog_group(group, downgrade).rstrip
        end.join("\n\n")
      end

      def self.build_changelog_group(group, downgrade)
        body = +''

        group.each do |ch|
          body << change_summary(ch)
        end

        reference = group.first
        return body unless reference.url && reference.old_rev && reference.new_rev
        return body if reference.old_rev == reference.new_rev

        from = downgrade ? reference.new_rev : reference.old_rev
        to = downgrade ? reference.old_rev : reference.new_rev

        begin
          mirror = ConfCtl::GitRepoMirror.new(reference.url, quiet: true)
          mirror.setup
          body << mirror.log(from, to, opts: ['--oneline'])
        rescue StandardError => e
          body << "(changelog unavailable: #{e})"
        end

        body
      end

      def self.group_changes(changes)
        groups = []
        group_indexes = {}

        changes.each do |ch|
          key = group_key(ch)

          if key && group_indexes.has_key?(key)
            groups[group_indexes[key]] << ch
          else
            group_indexes[key] = groups.length if key
            groups << [ch]
          end
        end

        groups
      end

      def self.group_key(change)
        url = normalize_url(change.url)
        return nil if url.nil?

        [url, change.old_rev, change.new_rev]
      end

      def self.change_summary(change)
        "#{change.name}: #{change.old_rev ? change.old_short_rev : 'set'} -> #{change.new_short_rev || '-'}\n"
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

      private_class_method :build_plain_body, :build_changelog_body, :build_changelog_group,
                           :group_changes, :group_key, :change_summary, :build_title,
                           :common_short_rev, :same_sources?, :normalize_url
    end
  end
end
