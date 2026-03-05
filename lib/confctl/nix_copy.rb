module ConfCtl
  class NixCopy
    # @param target [String]
    # @param port [Integer]
    # @param store_paths [Array<String>]
    def initialize(target, store_paths, port: 22)
      @target = target
      @store_paths = store_paths
      @port = port.to_i
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
        env: nix_ssh_env,
        &line_buf.feed_block
      )

      line_buf.flush
      ret
    end

    protected

    attr_reader :target, :store_paths, :port

    def nix_ssh_env
      return {} if port == 22

      ssh_opts = [ENV.fetch('NIX_SSHOPTS', nil), "-p #{port}"].compact.join(' ').strip
      {
        'NIX_SSHOPTS' => ssh_opts
      }
    end

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
