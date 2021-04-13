require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Core < Command
    include Swpins::Utils

    def list
      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      core = ConfCtl::Swpins::Core.new(channels)
      core.parse

      rows = []

      core.specs.each do |name, spec|
        next if args[0] && !ConfCtl::Pattern.match(args[0], name)

        rows << {
          sw: spec.name,
          channel: spec.channel,
          type: spec.type,
          pin: spec.status,
        }
      end

      OutputFormatter.print(
        rows,
        %i(sw channel type pin),
        layout: :columns,
      )
    end

    def set
      require_args!('sw', 'version...')

      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      core = ConfCtl::Swpins::Core.new(channels)
      core.parse

      core.specs.each do |name, spec|
        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        else
          puts "Configuring #{spec.name} in #{core.name}"
          spec.prefetch_set(args[1..-1])
        end
      end

      core.save
    end

    def update
      channels = ConfCtl::Swpins::ChannelList.new
      channels.each(&:parse)

      core = ConfCtl::Swpins::Core.new(channels)
      core.parse

      core.specs.each do |name, spec|
        next if args[0] && !ConfCtl::Pattern.match(args[0], name)

        if spec.from_channel?
          puts "Skipping #{spec.name} as it comes from channel #{spec.channel}"
        elsif spec.can_update?
          puts "Updating #{spec.name} in #{core.name}"
          spec.prefetch_update
        else
          puts "#{spec.name} not configured for update"
        end
      end

      core.save
    end
  end
end
