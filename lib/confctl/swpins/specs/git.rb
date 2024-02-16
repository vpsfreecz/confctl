require_relative 'base'
require 'json'
require 'time'

module ConfCtl
  class Swpins::Specs::Git < Swpins::Specs::Base
    handle :git

    def check_opts
      nix_opts['url'] === json_opts['nix_options']['url'] \
        && nix_opts['fetchSubmodules'] === json_opts['nix_options']['fetchSubmodules']
    end

    def version
      state['rev'][0..8]
    end

    def auto_update?
      super && (
        state['date'].nil? \
        || (Time.iso8601(state['date']) + nix_opts['update']['interval'] < Time.now)
      )
    end

    def prefetch_set(args)
      ref = args[0]

      if %r{^https://github\.com/} =~ nix_opts['url'] && !nix_opts['fetchSubmodules']
        set_fetcher('zip', prefetch_github(ref))
      else
        set_fetcher('git', prefetch_git(ref))
      end
    end

    def prefetch_update
      prefetch_set([nix_opts['update']['ref']])
    end

    def check_info(other_info)
      return false if !other_info.is_a?(Hash) || !info.is_a?(Hash)

      other_info['rev'] == info['rev'] && other_info['sha256'] == info['sha256']
    end

    def version_info(other_info)
      return false if !other_info.is_a?(Hash) || !info.is_a?(Hash)

      other_info['rev'] && other_info['rev'][0..8]
    end

    def string_changelog_info(type, other_info, verbose: false, patch: false, color: false)
      opts = []
      opts << '--color=always' if color
      opts << '--oneline' unless verbose
      opts << '-p' if patch

      args =
        if type == :upgrade
          [other_info['rev'], state['rev']]
        else
          [state['rev'], other_info['rev']]
        end

      git_mirror_with_info(other_info) do |mirror|
        mirror.log(*args, opts:)
      end
    end

    def string_diff_info(type, other_info, color: false, **_opts)
      gitopts = []
      gitopts << '--color=always' if color

      args =
        if type == :upgrade
          [other_info['rev'], state['rev']]
        else
          [state['rev'], other_info['rev']]
        end

      git_mirror_with_info(other_info) do |mirror|
        mirror.diff(*args, opts: gitopts)
      end
    end

    protected

    def prefetch_git(ref)
      json = `nix-prefetch-git --quiet #{nix_opts['url']} #{ref}`

      raise "nix-prefetch-git failed with status #{$?.exitstatus}" if $?.exitstatus != 0

      ret = JSON.parse(json.strip)
      set_state({
        'rev' => ret['rev'],
        'date' => Time.now.iso8601
      })
      set_info({
        'rev' => ret['rev'],
        'sha256' => ret['sha256']
      })
      ret
    end

    def prefetch_github(ref)
      mirror = GitRepoMirror.new(nix_opts['url'])
      mirror.setup

      rev = mirror.revision_parse(ref)
      url = File.join(nix_opts['url'], 'archive', "#{rev}.tar.gz")
      hash = `nix-prefetch-url --unpack "#{url}" 2> /dev/null`.strip

      raise "nix-prefetch-url failed with status #{$?.exitstatus}" if $?.exitstatus != 0

      set_state({
        'rev' => rev,
        'date' => Time.now.iso8601
      })
      set_info({
        'rev' => rev,
        'sha256' => hash
      })
      { 'url' => url, 'sha256' => hash }
    end

    def git_mirror_with_info(other_info)
      raise ConfCtl::Error, 'swpin not configured' if info.nil?

      return if state['rev'] == other_info['rev']

      mirror = GitRepoMirror.new(nix_opts['url'], quiet: true)
      mirror.setup
      yield(mirror)
    end
  end
end
