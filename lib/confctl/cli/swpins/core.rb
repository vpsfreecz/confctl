require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Core < Command
    include Swpins::Utils

    def list
      core = ConfCtl::Swpins::Core.get

      rows = []

      core.specs.each do |name, spec|
        next if args[0] && !ConfCtl::Pattern.match(args[0], name)

        rows << {
          sw: spec.name,
          channel: spec.channel,
          type: spec.type,
          pin: spec.status
        }
      end

      OutputFormatter.print(
        rows,
        %i[sw channel type pin],
        layout: :columns
      )
    end

    def set
      require_args!('sw', 'version...', strict: false)

      core = ConfCtl::Swpins::Core.get
      change_set = ConfCtl::Swpins::ChangeSet.new

      core.specs.each do |_name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        else
          change_set.add(core, spec)
          spec_set_msg(core, spec) { spec.prefetch_set(args[1..]) }
        end
      end

      core.save
      core.pre_evaluate

      return unless opts[:commit]

      change_set.commit(
        type: opts[:downgrade] ? :downgrade : :upgrade,
        changelog: opts[:changelog]
      )
    end

    def update
      require_args!(optional: %w[sw])
      core = ConfCtl::Swpins::Core.get
      change_set = ConfCtl::Swpins::ChangeSet.new

      core.specs.each do |name, spec|
        next if args[0] && !ConfCtl::Pattern.match(args[0], name)

        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        elsif spec.can_update?
          change_set.add(core, spec)
          spec_update_msg(core, spec) { spec.prefetch_update }
        else
          puts "#{spec.name} not configured for update"
        end
      end

      core.save
      core.pre_evaluate

      return unless opts[:commit]

      change_set.commit(
        type: opts[:downgrade] ? :downgrade : :upgrade,
        changelog: opts[:changelog]
      )
    end
  end
end
