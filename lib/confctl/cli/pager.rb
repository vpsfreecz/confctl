module ConfCtl::Cli
  class Pager
    def self.open(&block)
      pager = new
      pager.open(&block)
    end

    def open
      if ENV['PAGER'].nil? || ENV['PAGER'].strip.empty?
        return yield(STDOUT)
      end

      r, w = IO.pipe

      pid = Kernel.spawn(
        ENV['PAGER'],
        :in => r,
        w => :close,
        :close_others => true,
      )
      r.close

      Signal.trap('INT') {}

      ret = yield(w)
      w.close

      Process.wait(pid)
      Signal.trap('INT', 'DEFAULT')
      ret
    end
  end
end
