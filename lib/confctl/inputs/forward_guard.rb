require 'confctl/inputs/lock_graph'
require 'confctl/inputs/git_compare'

module ConfCtl
  module Inputs
    class ForwardGuard
      Result = Struct.new(:status, :changes, :issues, :rejections) do
        def success? = %i[unchanged updated].include?(status)
        def rejected? = status == :rejected
        def error? = status == :error
        def behind_only? = rejected? && !rejections.empty? && rejections.all? { |r| r.kind == :behind }
      end

      Change = Struct.new(:path, :old_node, :new_node) do
        def old_rev = old_node&.dig('locked', 'rev')
        def new_rev = new_node&.dig('locked', 'rev')
      end

      Rejection = Struct.new(:kind, :path, :old_rev, :new_rev)

      class UnsupportedSource < StandardError; end

      def initialize(provider: GitCompare.new)
        @provider = provider
      end

      def validate(old_lock, candidate_lock)
        old_entries = LockGraph.new(old_lock).entries
        new_entries = LockGraph.new(candidate_lock).entries
        changes = []
        rejected = []
        rejections = []
        errors = []
        cache = {}

        (old_entries.keys | new_entries.keys).sort.each do |path|
          old_node = old_entries[path]&.node
          new_node = new_entries[path]&.node
          next if same_source?(old_node, new_node)

          change = Change.new(path: path, old_node: old_node, new_node: new_node)
          changes << change
          check_change(change, rejected, rejections, cache)
        rescue UnsupportedSource, GitCompare::Error => e
          errors << "#{path}: #{e.message}"
        end

        status = if !errors.empty?
                   :error
                 elsif !rejected.empty?
                   :rejected
                 elsif changes.empty?
                   :unchanged
                 else
                   :updated
                 end
        Result.new(status: status, changes: changes, issues: errors + rejected, rejections: rejections)
      rescue LockGraph::Error => e
        Result.new(status: :error, changes: [], issues: [e.message], rejections: [])
      end

      protected

      def same_source?(old_node, new_node)
        old_node && new_node && old_node['locked'] == new_node['locked'] &&
          old_node.fetch('flake', true) == new_node.fetch('flake', true)
      end

      def check_change(change, rejected, rejections, cache)
        if change.new_node.nil?
          reject_change(change, :removed, "#{change.path}: previously reachable dependency was removed", rejected, rejections)
          return
        end

        new_source = source(change.new_node)
        return if change.old_node.nil?

        old_source = source(change.old_node)
        if old_source != new_source || change.old_node.fetch('flake', true) != change.new_node.fetch('flake', true)
          reject_change(change, :source_changed, "#{change.path}: source or extraction attributes changed; manual review required", rejected, rejections)
          return
        end

        if change.old_rev == change.new_rev
          reject_change(change, :revision_metadata_changed, "#{change.path}: locked attributes changed at the same revision", rejected, rejections)
          return
        end

        key = [old_source, change.old_rev, change.new_rev]
        outcome = cache.fetch(key) do
          cache[key] = begin
            @provider.compare(
              owner: change.new_node['locked']['owner'], repo: change.new_node['locked']['repo'],
              old_rev: change.old_rev, new_rev: change.new_rev
            )
          rescue GitCompare::Error => e
            e
          end
        end
        raise outcome if outcome.is_a?(GitCompare::Error)

        return if outcome == :ahead

        unless %i[behind diverged].include?(outcome)
          raise GitCompare::Error, 'Unknown ancestry comparison outcome'
        end

        reject_change(change, outcome, "#{change.path}: #{change.old_rev} -> #{change.new_rev} is #{outcome}", rejected, rejections)
      end

      def reject_change(change, kind, message, rejected, rejections)
        rejected << message
        rejections << Rejection.new(kind: kind, path: change.path, old_rev: change.old_rev, new_rev: change.new_rev)
      end

      def source(node)
        locked = node['locked']
        unless locked['type'] == 'github' && [nil, 'github.com'].include?(locked['host']) &&
               %w[owner repo].all? do |key|
                 value = locked[key]
                 value.is_a?(String) && GitCompare::NAME.match?(value) && !%w[. ..].include?(value)
               end
          raise UnsupportedSource, 'Changed dependency is not a supported github.com source'
        end
        unless locked['rev'].is_a?(String) && GitCompare::SHA.match?(locked['rev'])
          raise UnsupportedSource, 'Changed dependency lacks a full immutable Git revision'
        end

        allowed = %w[type owner repo host rev narHash lastModified dir]
        unless (locked.keys - allowed).empty? &&
               (!locked.has_key?('dir') || locked['dir'].is_a?(String)) &&
               (!locked.has_key?('narHash') || locked['narHash'].is_a?(String)) &&
               (!locked.has_key?('lastModified') || locked['lastModified'].is_a?(Integer))
          raise UnsupportedSource, 'Unsupported GitHub source attributes'
        end

        locked.except('rev', 'narHash', 'lastModified').merge(
          'owner' => locked['owner'].downcase, 'repo' => locked['repo'].downcase, 'host' => 'github.com'
        )
      end
    end
  end
end
