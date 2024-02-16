module ConfCtl
  class ParallelExecutor
    attr_reader :thread_count

    def initialize(threads)
      @thread_count = threads
      @threads = []
      @queue = Queue.new
      @retvals = []
      @mutex = Mutex.new
    end

    def add(&block)
      queue << block
    end

    def run
      thread_count.times do
        threads << Thread.new { worker }
      end

      threads.each(&:join)
      retvals
    end

    protected

    attr_reader :threads, :queue, :mutex, :retvals

    def worker
      loop do
        begin
          block = queue.pop(true)
        rescue ThreadError
          return
        end

        v = block.call
        mutex.synchronize { retvals << v }
      end
    end
  end
end
