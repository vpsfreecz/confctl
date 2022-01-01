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
      if args.any?
        fail "spec #{name} does not accept any arguments"
      end

      set_fetcher(:directory, {path: nix_opts['path']})
    end

    def prefetch_update
      set_fetcher(:directory, {path: nix_opts['path']})
    end

    def check_info(other_info)
      false
    end

    def version_info(other_info)
      'directory'
    end

    def string_changelog_info(type, other_info, verbose: false, patch: false, color: false)
      nil
    end

    def string_diff_info(type, other_info, opts = {})
      nil
    end
  end
end
