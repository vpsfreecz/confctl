require 'tty-command'

module ConfCtl
  module SystemCommand
    # @return [TTY::Command]
    def self.new
      TTY::Command.new(output: Logger.instance, color: false)
    end
  end
end
