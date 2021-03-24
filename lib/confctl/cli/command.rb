require 'gli'
require 'rainbow'

module ConfCtl::Cli
  class Command
    def self.run(klass, method)
      Proc.new do |global_opts, opts, args|
        cmd = klass.new(global_opts, opts, args)
        cmd.method(method).call
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

    protected
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
