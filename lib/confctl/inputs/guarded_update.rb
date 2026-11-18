require 'fileutils'
require 'open3'
require 'tmpdir'
require 'tty-command'
require 'confctl/exceptions'
require 'confctl/flake_lock'
require 'confctl/flake_lock_diff'
require 'confctl/inputs/lock_graph'
require 'confctl/inputs/forward_guard'

module ConfCtl
  module Inputs
    # The checkout must have a single writer. The process lock serializes
    # cooperating confctl updates; snapshots detect other writers before install.
    class GuardedUpdate
      def initialize(conf_dir:, inputs:, guard: ForwardGuard.new)
        @conf_dir = File.expand_path(conf_dir)
        @inputs = inputs
        @guard = guard
        @lock_path = File.join(@conf_dir, 'flake.lock')
      end

      def run!(after_install: nil)
        process_lock = git!('rev-parse', '--git-path', 'confctl-input-update.lock').strip
        File.open(File.expand_path(process_lock, @conf_dir), File::RDWR | File::CREAT, 0o600) do |lock|
          unless lock.flock(File::LOCK_EX | File::LOCK_NB)
            raise ConfCtl::Error, 'another guarded input update is running'
          end

          snapshot!
          old_lock = FlakeLock.new(@lock_path, JSON.parse(@original_lock))
          groups = selection_groups(LockGraph.new(old_lock))

          Dir.mktmpdir('.confctl-update-', @conf_dir) do |dir|
            reference = File.join(dir, 'reference.lock')
            output = File.join(dir, 'candidate.lock')
            File.binwrite(reference, @original_lock)
            accepted = old_lock
            skipped = []
            updated_inputs = []

            groups.each do |target, requested_inputs|
              verify_selections!(LockGraph.new(accepted), groups)
              FileUtils.rm_f(output)
              yield reference, output, [target]
              raise ConfCtl::Error, 'Nix did not produce a candidate lock' unless File.file?(output)

              candidate = FlakeLock.load(output)
              result = @guard.validate(accepted, candidate)
              if result.behind_only?
                skipped << { input: target, requested_inputs: requested_inputs, rejections: result.rejections }
                next
              end
              return outcome(result.status, issues: result.issues) unless result.success?

              verify_selections!(LockGraph.new(candidate), groups)
              next if result.status == :unchanged

              File.rename(output, reference)
              accepted = candidate
              updated_inputs.concat(requested_inputs)
            end

            result = @guard.validate(old_lock, accepted)
            return outcome(result.status, issues: result.issues) unless result.success?

            verify_snapshot!
            return outcome(:unchanged, skipped: skipped) if result.status == :unchanged

            changes = FlakeLockDiff.from_guard(result.changes, old_lock, accepted)
            File.chmod(@original_mode, reference)
            File.rename(reference, @lock_path)
            @installed = true
            after_install&.call(changes, updated_inputs)
            outcome(:updated, changes: changes, skipped: skipped, updated_inputs: updated_inputs)
          end
        end
      rescue StandardError => e
        # Commit hooks may have side effects. An installed, validated candidate
        # stays available for diagnosis if the subsequent commit fails.
        raise if @installed

        message = case e
                  when JSON::ParserError
                    'Invalid lock JSON'
                  when TTY::Command::ExitError
                    'Nix failed while preparing the candidate lock'
                  when ConfCtl::Error, LockGraph::Error
                    e.message
                  else
                    "Guarded input update failed (#{e.class})"
                  end
        outcome(:error, issues: [message])
      end

      protected

      def selection_groups(graph)
        @inputs.uniq.sort.group_by { |name| graph.resolve_path(name).terminal_path }.sort.to_h
      end

      def verify_selections!(graph, groups)
        groups.each do |target, names|
          names.each do |name|
            next if graph.resolve_path(name).terminal_path == target

            raise ConfCtl::Error, "selected input '#{name}' now resolves to a different target; retry from a fresh baseline"
          end
        end
      end

      def snapshot!
        unless File.file?(@lock_path) && !File.symlink?(@lock_path)
          raise ConfCtl::Error, 'forward-only updates require an existing regular flake.lock'
        end

        git!('ls-files', '--error-unmatch', '--', 'flake.lock')
        unless git!('status', '--porcelain=v1', '--untracked-files=no').empty?
          raise ConfCtl::Error, 'forward-only updates require a clean tracked checkout and index'
        end

        @original_lock = File.binread(@lock_path)
        @original_mode = File.stat(@lock_path).mode & 0o777
        @original_flake = File.binread(File.join(@conf_dir, 'flake.nix'))
        @head = git!('rev-parse', 'HEAD')
        @index = git!('ls-files', '--stage', '-z')
      end

      def verify_snapshot!
        unchanged = File.binread(@lock_path) == @original_lock &&
                    File.binread(File.join(@conf_dir, 'flake.nix')) == @original_flake &&
                    git!('rev-parse', 'HEAD') == @head &&
                    git!('ls-files', '--stage', '-z') == @index &&
                    git!('status', '--porcelain=v1', '--untracked-files=no').empty?
        return if unchanged

        raise ConfCtl::Error, 'checkout changed while preparing the input update; retry from a fresh baseline'
      end

      def git!(*args)
        out, _err, status = Open3.capture3({ 'GIT_OPTIONAL_LOCKS' => '0' }, 'git', *args, chdir: @conf_dir)
        raise ConfCtl::Error, "guarded update Git preflight failed (#{args.first})" unless status.success?

        out
      end

      def outcome(status, changes: [], issues: [], skipped: [], updated_inputs: [])
        { status: status, changed: status == :updated, changes: changes, issues: issues,
          skipped: skipped, updated_inputs: updated_inputs }
      end
    end
  end
end
