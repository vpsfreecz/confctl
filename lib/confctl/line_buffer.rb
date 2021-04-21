module ConfCtl
  class LineBuffer
    # @yieldparam out [String, nil]
    # @yieldparam err [String, nil]
    def initialize(&block)
      @out_buffer = ''
      @err_buffer = ''
      @block = block
    end

    def feed_block
      Proc.new do |stdout, stderr|
        out_buffer << stdout if stdout
        err_buffer << stderr if stderr

        loop do
          out_line, @out_buffer = get_line(@out_buffer)
          err_line, @err_buffer = get_line(@err_buffer)
          break if out_line.nil? && err_line.nil?

          block.call(out_line, err_line)
        end
      end
    end

    def flush
      block.call(out_buffer, err_buffer)
      out_buffer.clear
      err_buffer.clear
    end

    protected
    attr_reader :out_buffer, :err_buffer, :block

    def get_line(buf)
      nl = buf.index("\n")
      return nil, buf if nl.nil?

      [buf[0..nl], buf[nl+1..-1]]
    end
  end
end
