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

    # @param header [String]
    #   optional string outputted above the box, must have new lines
    # @param title [String]
    #   optional box title
    # @param size [Integer]
    #   number of lines to show
    def initialize(header: nil, title: nil, size: 10)
      @cursor = TTY::Cursor
      @outmutex = Mutex.new
      @inlines = Queue.new
      @outlines = []
      @header = header
      @title = title || 'Log'
      @size = size
      @resized = false
      @stop = false
      @generation = 0
      @rendered = 0
    end

    def start
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
      @stop = true
      inlines.clear
      inlines << :stop
      feeder.join
      renderer.join
      Signal.trap('WINCH', 'DEFAULT')
    end

    def <<(line)
      inlines << line.strip
    end

    def sync_console(&block)
      self.class.sync_console(&block)
    end

    protected
    attr_reader :cursor, :outmutex, :inlines, :outlines, :size,
      :feeder, :renderer, :rows, :cols, :header, :title,
      :generation, :rendered

    def feeder_loop
      loop do
        line = inlines.pop
        break if stop?

        sync_outlines do
          outlines << line
          outlines.shift if outlines.length > size
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
              print cursor.clear_screen
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
        rows.times { puts }
        print cursor.clear_screen
        print cursor.move_to
      end
    end

    def render_scoped(lines)
      sync_console do
        print cursor.save
        print cursor.move_to
        render_inplace(lines)
        print cursor.restore
      end
    end

    def render_inplace(lines)
      sync_console do
        if header
          header.each_line do |line|
            print cursor.clear_line
            print line
          end
        end

        print cursor.clear_line
        puts title_bar(title)

        size.times do |i|
          print cursor.clear_line

          if lines[i].nil?
            puts
            next
          end

          puts(fit_line(lines[i]))
        end

        print cursor.clear_line
        puts '<' + '-' * (cols-1)
        puts
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
      if line.length >= (cols - 6)
        line[0..(cols-6)] + "..."
      else
        line
      end
    end

    def fetch_size
      @rows, @cols = IO.console.winsize
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
