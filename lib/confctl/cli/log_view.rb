require 'io/console'
require 'rainbow'
require 'thread'
require 'tty-cursor'

module ConfCtl::Cli
  # Create a fixed-size box showing the last `n` lines from streamed data
  class LogView
    # All writes to the console must go through this lock
    CONSOLE_LOCK = Monitor.new

    def self.sync_console(&block)
      CONSOLE_LOCK.synchronize(&block)
    end

    # Instantiate {LogView}, yield and then cleanup
    # @yieldparam log_view [LogView]
    def self.open(*args)
      lw = new(*args)
      lw.start

      begin
        yield(lw)
      ensure
        lw.stop
      end
    end

    # Instantiate {LogView} with feed from {ConfCtl::Logger}, yield
    # and then cleanup
    # @yieldparam log_view [LogView]
    def self.open_with_logger(*args)
      lw = new(*args)
      lw.start

      lb = ConfCtl::LineBuffer.new { |line| lw << line }
      ConfCtl::Logger.instance.add_reader(lb)

      begin
        yield(lw)
      ensure
        ConfCtl::Logger.instance.remove_reader(lb)
        lw.stop
      end
    end

    # @param header [String]
    #   optional string outputted above the box, must have new lines
    # @param title [String]
    #   optional box title
    # @param size [Integer, :auto]
    #   number of lines to show
    # @param reserved_lines [Integer]
    #   number of reserved lines below the box when `size` is `:auto`
    # @param output [IO]
    def initialize(header: nil, title: nil, size: 10, reserved_lines: 0, output: STDOUT)
      @cursor = TTY::Cursor
      @outmutex = Mutex.new
      @inlines = Queue.new
      @outlines = []
      @header = header
      @title = title || 'Log'
      @size = size
      @current_size = size if size != :auto
      @reserved_lines = reserved_lines
      @output = output
      @enabled = output.respond_to?(:tty?) && output.tty?
      @resized = false
      @stop = false
      @generation = 0
      @rendered = 0
    end

    def start
      return unless enabled?

      @stop = false
      fetch_size
      init
      render_inplace(outlines)
      @feeder = Thread.new { feeder_loop }
      @renderer = Thread.new { renderer_loop }

      Signal.trap('WINCH') do
        fetch_size
        @resized = true
      end
    end

    def stop
      return if @stop || !enabled?

      @stop = true
      inlines.clear
      inlines << :stop
      feeder.join
      renderer.join
      Signal.trap('WINCH', 'DEFAULT')
    end

    def flush
      sleep(1)
    end

    def <<(line)
      inlines << line.strip
    end

    def sync_console(&block)
      self.class.sync_console(&block)
    end

    def enabled?
      @enabled
    end

    protected
    attr_reader :output, :cursor, :outmutex, :inlines, :outlines, :size,
      :current_size, :reserved_lines, :feeder, :renderer, :rows, :cols, :header, :title,
      :generation, :rendered

    def feeder_loop
      loop do
        line = inlines.pop
        break if stop?

        sync_outlines do
          # TABs have variable width, there's no way to correctly determine
          # their size, so we replace them with spaces.
          outlines << line.gsub("\t", "  ")
          outlines.shift while outlines.length > current_size
          @generation += 1
        end
      end
    end

    def renderer_loop
      loop do
        return if stop?

        lines = nil
        do_render = true

        sync_outlines do
          if generation == rendered && !resized?
            do_render = false
            next
          end

          lines = outlines.clone
          @rendered = generation
        end

        if do_render
          sync_console do
            if resized?
              output.print(cursor.clear_screen)
              @resized = false
            end

            render_scoped(lines)
          end
        end

        return if stop?
        sleep(0.1)
      end
    end

    def init
      sync_console do
        rows.times { output.puts }
        output.print(cursor.clear_screen)
        output.print(cursor.move_to)
      end
    end

    def render_scoped(lines)
      sync_console do
        output.print(cursor.save)
        output.print(cursor.move_to)
        render_inplace(lines)
        output.print(cursor.restore)
      end
    end

    def render_inplace(lines)
      sync_console do
        if header
          header.each_line do |line|
            output.print(cursor.clear_line)
            output.print(line)
          end
        end

        output.print(cursor.clear_line)
        output.puts(title_bar(title))

        current_size.times do |i|
          output.print(cursor.clear_line)

          if lines[i].nil?
            output.puts
            next
          end

          output.puts(fit_line(lines[i]))
        end

        output.print(cursor.clear_line)
        output.puts('<' + '-' * (cols-1))
        output.puts
      end
    end

    def title_bar(s)
      uncolored = Rainbow::StringUtils.uncolor(s)

      ret = ''
      ret << s
      ret << ' '
      ret << '-' * (cols - uncolored.length - 2)
      ret << '>'
      ret
    end

    def fit_line(line)
      if line.length >= (cols - 4)
        line[0..(cols-4)] + "..."
      else
        line
      end
    end

    def fetch_size
      @rows, @cols = IO.console.winsize

      if size == :auto
        new_size = rows
        new_size -= header.lines.count if header
        new_size -= reserved_lines
        @current_size = [new_size, 10].max
      end
    end

    def sync_outlines(&block)
      sync_mutex(outmutex, &block)
    end

    def sync_mutex(mutex, &block)
      if mutex.owned?
        block.call
      else
        mutex.synchronize(&block)
      end
    end

    def resized?
      @resized
    end

    def stop?
      @stop
    end
  end
end
