require 'confctl/cli/command'
require 'confctl/cli/swpins/utils'

module ConfCtl::Cli
  class Swpins::Channel < Command
    include Swpins::Utils

    def list
      rows = []

      each_channel_spec(args[0] || '*', args[1] || '*') do |chan, spec|
        rows << {
          channel: chan.name,
          sw: spec.name,
          type: spec.type,
          pin: spec.status,
        }
      end

      OutputFormatter.print(
        rows,
        %i(channel sw type pin),
        layout: :columns,
      )
    end

    def set
      require_args!('channel-pattern', 'sw-pattern', 'version...')
      channels = []

      each_channel_spec(args[0], args[1]) do |chan, spec|
        puts "Configuring #{spec.name} in channel #{chan.name}"
        spec.prefetch_set(args[2..])
        channels << chan unless channels.include?(chan)
      end

      channels.each(&:save)
    end

    def update
      channels = []

      each_channel_spec(args[0] || '*', args[1] || '*') do |chan, spec|
        if spec.can_update?
          puts "Updating #{spec.name} in channel #{chan.name}"
          spec.prefetch_update
          channels << chan unless channels.include?(chan)
        else
          puts "#{spec.name} not configured for update"
        end
      end

      channels.each(&:save)
    end
  end
end
