require_relative 'git'

module ConfCtl
  class Swpins::Specs::GitRev < Swpins::Specs::Git
    handle :'git-rev'

    def prefetch_set(args)
      super(args)
      wrap_fetcher
    end

    protected
    def wrap_fetcher
      set_fetcher('git-rev', {
        'rev' => state['rev'],
        'wrapped_fetcher' => {
          'type' => fetcher,
          'options' => fetcher_opts,
        }
      })
    end
  end
end
