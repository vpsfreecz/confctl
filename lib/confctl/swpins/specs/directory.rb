require_relative 'base'
require 'json'
require 'time'

module ConfCtl
  class Swpins::Specs::Directory < Swpins::Specs::Base
    handle :directory

    def check_opts
      nix_opts['path'] === json_opts['nix_options']['path']
    end

    def version
      'directory'
    end

    def can_update?
      true
    end

    def auto_update?
      true
    end

    def prefetch_set(args)
      raise "spec #{name} does not accept any arguments" if args.any?

      set_fetcher(:directory, { path: nix_opts['path'] })
    end

    def prefetch_update
      set_fetcher(:directory, { path: nix_opts['path'] })
    end

    def check_info(_other_info)
      false
    end

    def version_info(_other_info)
      'directory'
    end

    def string_changelog_info(_type, _other_info, verbose: false, patch: false, color: false)
      nil
    end

    def string_diff_info(_type, _other_info, _opts = {})
      nil
    end
  end
end
