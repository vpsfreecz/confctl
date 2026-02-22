require 'confctl/cli/command'
require 'confctl/config_type'
require 'confctl/flake_lock'
require 'confctl/pattern'
require 'confctl/pins/setter'
require 'confctl/pins/updater'

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
      inputs = if opts[:all]
                 lock = ConfCtl::FlakeLock.load_optional(File.join(conf_dir, 'flake.lock'))
                 raise ConfCtl::Error, 'flake.lock missing; specify inputs explicitly' if lock.nil?

                 lock.root_inputs
               else
                 raise GLI::BadCommandLine, 'missing input name (or pass --all)' if args.empty?

                 args
               end

      raise ConfCtl::Error, 'no inputs selected' if inputs.empty?

      res = ConfCtl::Pins::Updater.run!(
        conf_dir: conf_dir,
        inputs: inputs,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      print_update_summary(res[:changes])
    end

    def set
      ensure_flake_config!
      require_args!('input-name', 'rev')

      input = args[0]
      rev = args[1]

      res = ConfCtl::Pins::Setter.run!(
        conf_dir: ConfCtl::ConfDir.path,
        inputs: [input],
        rev: rev,
        commit: opts[:commit],
        changelog: opts[:changelog],
        downgrade: opts[:downgrade],
        editor: opts[:editor]
      )

      print_update_summary(res[:changes])
    end

    protected

    def ensure_flake_config!
      return if ConfCtl::ConfigType.flake?(ConfCtl::ConfDir.path)

      raise ConfCtl::Error, 'pins is for flake configs only; this config has no flake.nix; use swpins.'
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
  end
end
