require 'fileutils'
require 'pathname'
require 'pp'
require 'singleton'

module ConfCtl
  class Logger
    class << self
      %i[
        open
        close
        unlink
        close_and_unlink
        keep?
        keep
        io
        path
        relative_path
      ].each do |v|
        define_method(v) { |*args, **kwargs| instance.send(v, *args, **kwargs) }
      end
    end

    include Singleton

    def initialize
      @mutex = Mutex.new
      @readers = []
      @keep = false
    end

    def open(name, output: nil)
      if output
        @io = output
      else
        dir = ConfDir.log_dir
        FileUtils.mkdir_p(dir)

        @io = File.new(File.join(dir, file_name(name)), 'w')
      end
    end

    def open?
      !@io.nil?
    end

    def close
      raise 'log file not open' if @io.nil?

      @io.close
    end

    def io
      raise 'log file not open' if @io.nil?

      @io
    end

    def path
      raise 'log file not open' if @io.nil?

      @io.path
    end

    def relative_path
      return @relative_path if @relative_path

      abs = Pathname.new(path)
      dir = Pathname.new(ConfDir.path)
      abs.relative_path_from(dir).to_s
    end

    def keep?
      @keep
    end

    def keep
      @keep = true
    end

    def unlink
      raise 'log file not open' if @io.nil?

      File.unlink(@io.path)
    end

    def close_and_unlink
      close
      unlink
    end

    def write(str)
      sync do
        @io << str
        @io.flush
      end

      readers.each { |r| r << str }
    end

    def <<(str)
      write(str)
    end

    def cli(cmd, gopts, opts, args)
      sync do
        PP.pp({
          command: cmd,
          global_options: prune_opts(gopts.clone),
          command_options: prune_opts(opts.clone),
          arguments: args
        }, @io)
      end
    end

    # @param obj [#<<]
    def add_reader(obj)
      @readers << obj
    end

    # @param obj [#<<]
    def remove_reader(obj)
      @readers.delete(obj)
    end

    protected

    attr_reader :mutex, :readers

    def sync(&)
      if mutex.owned?
        yield
      else
        mutex.synchronize(&)
      end
    end

    def file_name(name)
      n = [
        Time.now.strftime('%Y-%m-%d--%H-%M-%S'),
        'confctl',
        name
      ].compact.join('-')
      "#{n}.log"
    end

    def prune_opts(hash)
      hash.delete_if { |k, _v| k.is_a?(::Symbol) }
    end
  end
end
