require 'thread'

module ConfCtl
  class ParallelExecutor
    attr_reader :thread_count

    def initialize(threads)
      @thread_count = threads
      @threads = []
      @queue = Queue.new
    end

    def add(&block)
      queue << block
    end

    def run
      thread_count.times do
        threads << Thread.new { worker }
      end

      threads.each(&:join)
    end

    protected
    attr_reader :threads, :queue

    def worker
      loop do
        begin
          block = queue.pop(true)
        rescue ThreadError
          return
        end

        block.call
      end
    end
  end
end
