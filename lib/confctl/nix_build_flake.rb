module ConfCtl
  class NixBuildFlake
    # @param args [Array<String>]
    # @param chdir [String]
    def initialize(args, chdir:)
      @args = args
      @chdir = chdir
      @build_progress = 0
      @build_total = 0
      @fetch_progress = 0
      @fetch_total = 0
    end

    # @yieldparam type [:build, :fetch]
    # @yieldparam progress [Integer]
    # @yieldparam total [Integer]
    # @yieldparam path [String]
    def run(&block)
      cmd = SystemCommand.new

      line_buf = StdLineBuffer.new do |_out, err|
        parse_line(err, &block) if err && block
      end

      ret = cmd.run('nix', 'build', *args, chdir: chdir, &line_buf.feed_block)

      line_buf.flush
      ret
    end

    protected

    attr_reader :args, :chdir

    def parse_line(line)
      # Beware that nix can fetch/build stuff even before those two
      # summary lines are printed. Therefore we report progress with total=0
      # (indeterminate) until the total becomes known.

      # Nix >= around 2.11
      case line
      when /^this derivation will be built:/
        @build_total = 1
        return
      when /^these (\d+) derivations will be built:/
        @build_total = ::Regexp.last_match(1).to_i
        return
      when /^this path will be fetched /
        @fetch_total = 1
        return
      when /^these (\d+) paths will be fetched /
        @fetch_total = ::Regexp.last_match(1).to_i
        return
      end

      # Nix < around 2.11, we can drop this in the future
      if /^these derivations will be built:/ =~ line
        @in_derivation_list = true
        @in_fetch_list = false
        return
      elsif /^these paths will be fetched / =~ line
        @in_derivation_list = false
        @in_fetch_list = true
        return
      end

      if @in_derivation_list
        if %r{^\s+/nix/store/} =~ line
          @build_total += 1
          return
        else
          @in_derivation_list = false
          @build_total += @build_progress
        end

      elsif @in_fetch_list
        if %r{^\s+/nix/store/} =~ line
          @fetch_total += 1
          return
        else
          @in_fetch_list = false
          @fetch_total += @fetch_progress
        end
      end

      if /^building '([^']+)/ =~ line
        @build_progress += 1
        yield(:build, @build_progress, @build_total, ::Regexp.last_match(1))
      elsif /^copying path '([^']+)/ =~ line
        @fetch_progress += 1
        yield(:fetch, @fetch_progress, @fetch_total, ::Regexp.last_match(1))
      end
    end
  end
end
