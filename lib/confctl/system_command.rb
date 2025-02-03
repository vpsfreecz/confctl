require 'tty-command'

module ConfCtl
  module SystemCommand
    # @param logger [#<<]
    # @return [TTY::Command]
    def self.new(logger: nil)
      TTY::Command.new(output: logger || Logger.instance, color: false)
    end
  end
end
