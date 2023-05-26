module ConfCtl
  # Run nix-collect-garbage on {Machine}
  class NixCollectGarbage
    class Progress
      def initialize(line)
        @line = line
        @path = false
        parse
      end

      def path?
        !@path.nil?
      end

      # @return [String, nil]
      def path
        @path
      end

      def to_s
        @line
      end

      protected
      def parse
        if /^deleting '(\/nix\/store\/[^']+)'$/ =~ @line
          @path = $1
        end
      end
    end

    # @param machine [String]
    def initialize(machine)
      @machine = machine
    end

    # @yieldparam progress [Progress]
    # @return [TTY::Command::Result]
    def run!
      mc = MachineControl.new(machine)

      line_buf = StdLineBuffer.new do |out, err|
        next unless block_given?

        # %d store paths deleted, %f MiB freed
        yield(Progress.new(out)) if out

        # finding garbage collector roots...
        # removing stale link from '...'
        # deleting garbage...
        # deleting '/nix/store/...'
        # ...
        yield(Progress.new(err)) if err
      end

      ret = mc.execute!('nix-collect-garbage', &line_buf.feed_block)
      line_buf.flush
      ret
    end

    protected
    attr_reader :machine
  end
end
