require 'fileutils'
require 'pp'
require 'singleton'
require 'thread'

module ConfCtl
  class Logger
    class << self
      %i(open close unlink close_and_unlink io path).each do |v|
        define_method(v) { instance.send(v) }
      end
    end

    include Singleton

    def initialize
      @mutex = Mutex.new
    end

    def open(name)
      dir = ConfDir.log_dir
      FileUtils.mkdir_p(dir)

      @io = File.new(File.join(dir, file_name(name)), 'w')
    end

    def open?
      !@io.nil?
    end

    def close
      fail 'log file not open' if @io.nil?
      @io.close
    end

    def io
      fail 'log file not open' if @io.nil?
      @io
    end

    def path
      fail 'log file not open' if @io.nil?
      @io.path
    end

    def unlink
      fail 'log file not open' if @io.nil?
      File.unlink(@io.path)
    end

    def close_and_unlink
      close
      unlink
    end

    def write(str)
      sync { @io << str }
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
          arguments: args,
        }, @io)
      end
    end

    protected
    attr_reader :mutex

    def sync
      if mutex.owned?
        yield
      else
        mutex.synchronize { yield }
      end
    end

    def file_name(name)
      n = [
        'confctl',
        name,
        Time.now.strftime('%Y-%m-%d--%H-%M-%S'),
      ].compact.join('-')
      "#{n}.log"
    end

    def prune_opts(hash)
      hash.delete_if { |k, v| k.is_a?(::Symbol) }
    end
  end
end
