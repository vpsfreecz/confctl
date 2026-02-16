require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/flake_lock'
require 'confctl/flake_lock_diff'
require 'confctl/pins'
require 'confctl/pattern'
require 'confctl/system_command'

module ConfCtl::Cli
  class Pins::Inputs < Command
    def list
      ensure_flake_config!

      lock = ConfCtl::FlakeLock.load(File.join(ConfCtl::ConfDir.path, 'flake.lock'))
      rows = lock.root_inputs.map do |input_name|
        info = lock.input_info(input_name)
        {
          input: input_name,
          type: info[:type],
          ref: info[:ref],
          rev: info[:short_rev],
          url: info[:url]
        }
      end

      if args[0]
        rows.select! { |r| ConfCtl::Pattern.match?(args[0], r[:input]) }
      end

      OutputFormatter.print(rows, %i[input type ref rev url], layout: :columns)
    end

    def update
      ensure_flake_config!

      conf_dir = ConfCtl::ConfDir.path
      lock_path = File.join(conf_dir, 'flake.lock')

      inputs = if opts[:all]
                 nil
               else
                 raise GLI::BadCommandLine, 'missing input name (or pass --all)' if args.empty?

                 args
               end

      old_lock = ConfCtl::FlakeLock.load_optional(lock_path)

      run_nix_flake_update!(conf_dir, inputs)

      new_lock = ConfCtl::FlakeLock.load(lock_path)

      changes = ConfCtl::FlakeLockDiff.diff(old_lock, new_lock, inputs: inputs)

      print_update_summary(changes)

      return if changes.empty?
      return unless opts[:commit]

      msg = ConfCtl::Pins::CommitMessage.build(
        changes: changes,
        changelog: opts[:changelog],
        downgrade: opts[:downgrade]
      )

      ConfCtl::Pins::GitCommit.commit!(
        conf_dir: conf_dir,
        message: msg,
        editor: opts[:editor],
        files: ['flake.lock']
      )
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'pins is for flake configs only; this config has no flake.nix; use swpins.'
    end

    def run_nix_flake_update!(conf_dir, inputs)
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

    def print_update_summary(changes)
      if changes.empty?
        puts 'No changes.'
        return
      end

      puts "Updated inputs (#{changes.length}):"
      changes.each do |ch|
        puts "  #{ch.name}: #{ch.old_short_rev || '-'} -> #{ch.new_short_rev || '-'}"
      end
    end

    def experimental_error?(message)
      message.match?(/experimental/i) && message.match?(/nix-command|flakes/i)
    end
  end
end
