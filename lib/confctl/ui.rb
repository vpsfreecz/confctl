module ConfCtl
  module Ui
    module_function

    # Return true when interactive TTY UI features should be enabled.
    #
    # Set CONFCTL_TTY=0 to force non-interactive output.
    def tty?
      return false if ENV['CONFCTL_TTY'] == '0'

      $stdout.tty? && $stderr.tty?
    end

    def no_color?
      v = ENV.fetch('NO_COLOR', nil)
      !v.nil? && !v.strip.empty?
    end
  end
end
