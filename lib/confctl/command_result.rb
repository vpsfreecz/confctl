module ConfCtl
  class CommandResult
    # @return [String]
    attr_reader :command

    # @return [Integer]
    attr_reader :exitstatus

    # @return [String, nil]
    attr_reader :output

    # @param command [Array, String]
    # @param exitstatus [Integer]
    def initialize(command, exitstatus, output: nil)
      if command.is_a?(Array)
        @command = command.map { |v| "\"#{v}\"" }.join(' ')
      else
        @command = command
      end

      @exitstatus = exitstatus
      @output = output
    end

    def success?
      exitstatus == 0
    end

    def failed?
      exitstatus != 0
    end
  end
end
