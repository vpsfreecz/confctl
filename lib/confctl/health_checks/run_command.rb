require 'confctl/health_checks/base'

module ConfCtl
  class HealthChecks::RunCommand < HealthChecks::Base
    class Output
      # @return [String, nil]
      attr_reader :match

      # @return [Array<String>]
      attr_reader :include

      # @return [Array<String>]
      attr_reader :exclude

      def initialize(opts)
        @match = opts['match']
        @include = opts['include']
        @exclude = opts['exclude']
      end

      # Returns nil if there is a match
      # @return [String, nil]
      def check_match(str)
        if @match.nil? || str == @match
          nil
        else
          @match
        end
      end

      # Returns the string that should be and is not included
      # @return [String, nil]
      def check_include(str)
        @include.each do |inc|
          return inc unless str.include?(inc)
        end

        nil
      end

      # Returns the string that is and should not be included
      # @return [String, nil]
      def check_exclude(str)
        @exclude.each do |exc|
          return exc if str.include?(exc)
        end

        nil
      end
    end

    class Command
      # @return [String]
      attr_reader :description

      # @return [Array<String>]
      attr_reader :command

      # @return [Integer]
      attr_reader :exit_status

      # @return [Output]
      attr_reader :stdout

      # @return [Output]
      attr_reader :stderr

      # @return [Integer]
      attr_reader :timeout

      # @return [Integer]
      attr_reader :cooldown

      def initialize(machine, opts)
        @description = opts['description']
        @command = make_command(machine, opts['command'])
        @exit_status = opts['exitStatus']
        @stdout = Output.new(opts['standardOutput'])
        @stderr = Output.new(opts['standardError'])
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
            machine[::Regexp.last_match(1)]
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

    def description
      if @command.description.empty?
        @command.to_s
      else
        @command.description
      end
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

      check_output(result)
    end

    def run_local_check
      cmd = SystemCommand.new
      result = cmd.run!(*@command.command)

      if result.status != @command.exit_status
        add_error("#{@command} failed with #{result.status} (#{@command.description})")
      end

      check_output(result)
    end

    def check_output(result)
      # stdout
      if (fragment = @command.stdout.check_match(result.out))
        add_error("#{@command}: standard output does not match #{fragment.inspect}")

      elsif (fragment = @command.stdout.check_include(result.out))
        add_error("#{@command}: standard output does not include #{fragment.inspect}")

      elsif (fragment = @command.stdout.check_exclude(result.out))
        add_error("#{@command}: standard output includes #{fragment.inspect}")

      # stderr
      elsif (fragment = @command.stderr.check_match(result.err))
        add_error("#{@command}: standard error does not match #{fragment.inspect}")

      elsif (fragment = @command.stderr.check_include(result.err))
        add_error("#{@command}: standard error does not include #{fragment.inspect}")

      elsif (fragment = @command.stderr.check_exclude(result.err))
        add_error("#{@command}: standard error includes #{fragment.inspect}")
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
