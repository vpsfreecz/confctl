module ConfCtl
  class Error < ::StandardError ; end

  class CommandFailed < Error
    # @return [CommandResult]
    attr_reader :command_result

    # @param result [CommandResult]
    def initialize(result)
      @command_result = result
      super("Command '#{result.command}' failed with exit status #{result.exitstatus}")
    end
  end
end
