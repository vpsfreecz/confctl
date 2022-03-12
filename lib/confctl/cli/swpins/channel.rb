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
      require_args!('channel-pattern', 'sw-pattern', 'version...', strict: false)
      channels = []

      each_channel_spec(args[0], args[1]) do |chan, spec|
        spec_set_msg(chan, spec) { spec.prefetch_set(args[2..-1]) }
        channels << chan unless channels.include?(chan)
      end

      channels.each(&:save)
    end

    def update
      require_args!(optional: %w(channel-pattern sw-pattern))
      channels = []

      each_channel_spec(args[0] || '*', args[1] || '*') do |chan, spec|
        if spec.can_update?
          spec_update_msg(chan, spec) { spec.prefetch_update }
          channels << chan unless channels.include?(chan)
        else
          puts "#{spec.name} not configured for update"
        end
      end

      channels.each(&:save)
    end
  end
end
