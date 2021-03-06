require 'gli'
require 'rainbow'

module ConfCtl::Cli
  class Command
    def self.run(gli_cmd, klass, method)
      Proc.new do |global_opts, opts, args|
        log = ConfCtl::Logger.instance
        log.open(gli_cmd.name_for_help.join('-'))
        log.cli(
          gli_cmd.name_for_help,
          global_opts,
          opts,
          args,
        )

        cmd = klass.new(global_opts, opts, args)
        cmd.run_method(method)

        log.close_and_unlink
      end
    end

    attr_reader :gopts, :opts, :args

    def initialize(global_opts, opts, args)
      @gopts = global_opts
      @opts = opts
      @args = args
      @use_color = determine_color
      @use_pager = determine_pager
    end

    # @param v [Array] list of required arguments
    def require_args!(*v)
      if v.count == 1 && v.first.is_a?(Array)
        required = v.first
      else
        required = v
      end

      return if args.count >= required.count

      arg = required[ args.count ]
      raise GLI::BadCommandLine, "missing argument <#{arg}>"
    end

    def use_color?
      @use_color
    end

    def use_pager?
      @use_pager
    end

    def ask_confirmation(always: false)
      return true if !always && opts[:yes]

      yield if block_given?
      STDOUT.write("\nContinue? [y/N]: ")
      STDOUT.flush
      ret = STDIN.readline.strip.downcase == 'y'
      puts
      ret
    end

    def ask_confirmation!(**kwargs, &block)
      fail 'Aborted' unless ask_confirmation(**kwargs, &block)
    end

    def run_method(method)
      self.method(method).call
    end

    protected
    def run_command(klass, method)
      c = klass.new(gopts, opts, args)
      c.run_method(method)
    end

    def determine_color
      case gopts[:color]
      when 'always'
        Rainbow.enabled = true
        true
      when 'never'
        Rainbow.enabled = false
        false
      when 'auto'
        Rainbow.enabled
      end
    end

    def determine_pager
      ENV['PAGER'] && ENV['PAGER'].strip != ''
    end
  end
end
