require 'confctl/flake_lock'
require 'confctl/flake_lock_diff'
require 'confctl/inputs/commit_message'
require 'confctl/inputs/git_commit'
require 'confctl/system_command'

module ConfCtl
  module Inputs
    class Updater
      def self.run!(conf_dir:, inputs:, commit:, changelog:, downgrade:, editor:)
        raise ArgumentError, 'inputs empty' if inputs.nil? || inputs.empty?

        lock_path = File.join(conf_dir, 'flake.lock')
        old_lock = ConfCtl::FlakeLock.load_optional(lock_path)

        run_nix_flake_update!(conf_dir, inputs)

        new_lock = ConfCtl::FlakeLock.load(lock_path)
        changes = ConfCtl::FlakeLockDiff.diff(old_lock, new_lock, inputs: inputs)

        return { changed: false, changes: [] } if changes.empty?

        if commit
          msg = ConfCtl::Inputs::CommitMessage.build(
            changes: changes,
            changelog: changelog,
            downgrade: downgrade
          )
          ConfCtl::Inputs::GitCommit.commit!(
            conf_dir: conf_dir,
            message: msg,
            editor: editor,
            files: ['flake.lock']
          )
        end

        { changed: true, changes: changes }
      end

      def self.run_nix_flake_update!(conf_dir, inputs)
        cmd = ConfCtl::SystemCommand.new
        extra_experimental = false

        loop do
          args = ['nix']
          if extra_experimental
            args << '--extra-experimental-features' << 'nix-command'
            args << '--extra-experimental-features' << 'flakes'
          end
          args << 'flake' << 'update'
          args.concat(inputs) if inputs

          begin
            Dir.chdir(conf_dir) { cmd.run(*args) }
            break
          rescue TTY::Command::ExitError => e
            if !extra_experimental && experimental_error?(e.message)
              extra_experimental = true
              next
            end

            raise
          end
        end
      end

      def self.experimental_error?(message)
        message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
      end

      private_class_method :run_nix_flake_update!, :experimental_error?
    end
  end
end
