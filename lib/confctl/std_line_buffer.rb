module ConfCtl
  # Pair of line buffers for standard output/error
  class StdLineBuffer
    # @yieldparam out [String, nil]
    # @yieldparam err [String, nil]
    def initialize(&block)
      @out_buffer = LineBuffer.new
      @err_buffer = LineBuffer.new
      @block = block
      @mutex = Mutex.new
    end

    # Get a block which can be called to feed data to the buffer
    # @return [Proc]
    def feed_block
      proc do |stdout, stderr|
        @mutex.synchronize do
          out_buffer << stdout if stdout
          err_buffer << stderr if stderr

          loop do
            out_line = out_buffer.read_line
            err_line = err_buffer.read_line
            break if out_line.nil? && err_line.nil?

            block.call(out_line, err_line)
          end
        end
      end
    end

    def flush
      block.call(out_buffer.flush, err_buffer.flush)
    end

    protected

    attr_reader :out_buffer, :err_buffer, :block
  end
end
