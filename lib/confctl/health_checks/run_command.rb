require 'confctl/health_checks/base'

module ConfCtl
  class HealthChecks::RunCommand < HealthChecks::Base
    class Command
      # @return [String]
      attr_reader :description

      # @return [Array<String>]
      attr_reader :command

      # @return [Integer]
      attr_reader :exit_status

      # @return [Integer]
      attr_reader :timeout

      # @return [Integer]
      attr_reader :cooldown

      def initialize(machine, opts)
        @description = opts['description']
        @command = make_command(machine, opts['command'])
        @exit_status = opts['exitStatus']
        @timeout = opts['timeout']
        @cooldown = opts['cooldown']
      end

      def to_s
        @command.join(' ')
      end

      protected
      def make_command(machine, args)
        args.map do |arg|
          arg.gsub(/\{([^\}]+)\}/) do
            machine[$1]
          end
        end
      end
    end

    # @param machine [Machine]
    # @param command [Command]
    # @param remote [Boolean]
    def initialize(machine, command, remote:)
      super(machine)
      @command = command
      @remote = remote
    end

    protected
    def run_check
      if @remote
        run_remote_check
      else
        run_local_check
      end
    end

    def run_remote_check
      mc = MachineControl.new(machine)
      result = mc.execute!(*@command.command)

      if result.status != @command.exit_status
        add_error("#{@command} failed with #{result.status} (#{@command.description})")
      end
    end

    def run_local_check
      cmd = SystemCommand.new
      result = cmd.run!(*@command.command)

      if result.status != @command.exit_status
        add_error("#{@command} failed with #{result.status} (#{@command.description})")
      end
    end

    def timeout?(time)
      started_at + @command.timeout < time
    end

    def cooldown
      @command.cooldown
    end
  end
end
