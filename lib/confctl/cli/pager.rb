module ConfCtl::Cli
  class Pager
    def self.open(&block)
      pager = new
      pager.open(&block)
    end

    def open
      if ENV['PAGER'].nil? || ENV['PAGER'].strip.empty?
        yield(STDOUT)
        return
      end

      r, w = IO.pipe

      begin
        pid = Kernel.spawn(
          ENV['PAGER'],
          :in => r,
          w => :close,
          :close_others => true,
        )
      rescue SystemCallError => e
        r.close
        w.close
        raise ConfCtl::Error, "unable to spawn pager: #{e.message} (#{e.class})"
      end

      r.close

      Signal.trap('INT') {}

      begin
        yield(w)
      rescue Errno::EPIPE
        # the pager was closed prematurely
      end

      w.close

      Process.wait(pid)
      Signal.trap('INT', 'DEFAULT')
      nil
    end
  end
end
