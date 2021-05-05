module ConfCtl
  # Feed string data and get output as lines
  class LineBuffer
    # If instantiated with a block, the block is invoked for each read line
    # @yieldparam line [String]
    def initialize(&block)
      @buffer = ''
      @block = block
    end

    # Feed string
    # @param str [String]
    def <<(str)
      buffer << str
      return if block.nil?

      loop do
        out_line = get_line
        break if out_line.nil?

        block.call(out_line)
      end
    end

    # Read one line if there is one
    # @return [String, nil]
    def get_line
      nl = buffer.index("\n")
      return if nl.nil?

      line = buffer[0..nl]
      @buffer = buffer[nl+1..-1]
      line
    end

    # Return the buffer's contents and flush it
    #
    # If block was given to {LineBuffer}, it will be invoked with the buffer
    # contents.
    #
    # @return [String]
    def flush
      ret = buffer.clone
      buffer.clear
      block.call(ret) if block
      ret
    end

    protected
    attr_reader :buffer, :block
  end
end
