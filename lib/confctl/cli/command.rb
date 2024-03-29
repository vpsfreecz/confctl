require 'gli'
require 'rainbow'

module ConfCtl::Cli
  class Command
    def self.run(gli_cmd, klass, method)
      proc do |global_opts, opts, args|
        log = ConfCtl::Logger.instance
        log.open(gli_cmd.name_for_help.join('-'))
        log.cli(
          gli_cmd.name_for_help,
          global_opts,
          opts,
          args
        )

        cmd = klass.new(global_opts, opts, args)
        cmd.run_method(method)

        if log.keep?
          log.close
        else
          log.close_and_unlink
        end
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
    # @param optional [Array] list of optional arguments
    # @param strict [Boolean] do not allow more arguments than specified
    def require_args!(*required, optional: [], strict: true)
      if args.count < required.count
        arg = required[args.count]
        raise GLI::BadCommandLine, "missing argument <#{arg}>"

      elsif strict && args.count > (required.count + optional.count)
        unknown = args[(required.count + optional.count)..]

        msg = ''

        msg << if unknown.count > 1
                 'unknown arguments: '
               else
                 'unknown argument: '
               end

        msg << unknown.join(' ')

        msg << ' (note that options must come before arguments)' if unknown.detect { |v| v.start_with?('-') }

        raise GLI::BadCommandLine, msg
      end
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

      loop do
        $stdout.write("\nContinue? [y/N]: ")
        $stdout.flush

        case $stdin.readline.strip.downcase
        when 'y'
          puts
          return true
        when 'n'
          puts
          return false
        end
      end
    end

    def ask_confirmation!(...)
      raise 'Aborted' unless ask_confirmation(...)
    end

    # @param options [Hash<String, String>] key => description
    # @param default [String] default option key
    # @return [String] selection option key
    def ask_action(options:, default:)
      yield if block_given?

      loop do
        $stdout.puts("\nOptions:\n")

        options.each do |key, desc|
          $stdout.puts("  [#{key}] #{desc}")
        end

        keys = options.keys.map do |k|
          if k == default
            k.upcase
          else
            k
          end
        end.join('/')

        $stdout.write("\nAction: [#{keys}]: ")
        $stdout.flush

        answer = $stdin.readline.strip.downcase

        if options.has_key?(answer)
          puts
          return answer
        end
      end
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
      ENV.fetch('PAGER', nil) && ENV['PAGER'].strip != ''
    end

    def select_machines(pattern)
      machines = ConfCtl::MachineList.new(show_trace: opts['show-trace'])

      attr_filters = AttrFilters.new(opts[:attr])
      tag_filters = TagFilters.new(opts[:tag])

      machines.select do |host, d|
        (pattern.nil? || ConfCtl::Pattern.match?(pattern, host)) \
          && attr_filters.pass?(d) \
          && tag_filters.pass?(d)
      end
    end

    def select_machines_with_managed(pattern)
      selected = select_machines(pattern)

      case opts[:managed]
      when 'n', 'no'
        selected.unmanaged
      when 'a', 'all'
        selected
      else
        selected.managed
      end
    end

    def list_machines(machines, prepend_cols: [])
      cols =
        if opts[:output]
          opts[:output].split(',')
        else
          ConfCtl::Settings.instance.list_columns
        end

      cols = prepend_cols + cols if prepend_cols

      rows = machines.map do |_host, machine|
        cols.to_h { |c| [c, machine[c]] }
      end

      OutputFormatter.print(
        rows,
        cols,
        header: !opts['hide-header'],
        layout: :columns
      )
    end
  end
end
