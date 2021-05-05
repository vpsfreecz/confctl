module ConfCtl
  class NixCopy
    # @param target [String]
    # @param store_path [String]
    def initialize(target, store_path)
      @target = target
      @store_path = store_path
      @total = nil
      @progress = 0
    end

    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    def run!(&block)
      cmd = SystemCommand.new

      line_buf = StdLineBuffer.new do |out, err|
        parse_line(err, &block) if err && block
      end

      ret = cmd.run!(
        'nix-copy-closure',
        '--to', "root@#{target}",
        store_path,
        &line_buf.feed_block
      )

      line_buf.flush
      ret
    end

    protected
    attr_reader :target, :store_path

    def parse_line(line)
      if @total.nil? && /^copying (\d+) paths/ =~ line
        @total = $1.to_i
        return
      end

      if /^copying path '([^']+)/ =~ line
        @progress += 1
        yield(@progress, @total, $1)
      end
    end
  end
end
