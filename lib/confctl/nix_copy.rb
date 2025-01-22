module ConfCtl
  class NixCopy
    # @param target [String]
    # @param store_paths [Array<String>]
    def initialize(target, store_paths)
      @target = target
      @store_paths = store_paths
      @total = nil
      @progress = 0
    end

    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    def run!(&block)
      cmd = SystemCommand.new

      line_buf = StdLineBuffer.new do |_out, err|
        parse_line(err, &block) if err && block
      end

      ret = cmd.run!(
        'nix-copy-closure',
        '--to', "root@#{target}",
        *store_paths,
        &line_buf.feed_block
      )

      line_buf.flush
      ret
    end

    protected

    attr_reader :target, :store_paths

    def parse_line(line)
      if @total.nil? && /^copying (\d+) paths/ =~ line
        @total = ::Regexp.last_match(1).to_i
        return
      end

      return unless /^copying path '([^']+)/ =~ line

      @progress += 1
      yield(@progress, @total, ::Regexp.last_match(1))
    end
  end
end
