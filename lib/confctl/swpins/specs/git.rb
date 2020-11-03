require 'json'

module ConfCtl
  class Swpins::Specs::Git < Swpins::Specs::Base
    handle :git

    def version
      spec_opts[:rev] && spec_opts[:rev][0..8]
    end

    # @param override_opts [Hash]
    # @option override_opts [String] :ref
    def prefetch(override_opts)
      if /^https:\/\/github\.com\// =~ spec_opts[:url] && !spec_opts[:fetch_submodules]
        gopts[:fetcher] = 'zip'
        prefetch_github(override_opts)
      else
        gopts[:fetcher] = 'git'
        prefetch_git(override_opts)
      end
    end

    protected
    def prefetch_git(override_opts)
      ref = override_opts[:ref]
      json = `nix-prefetch-git --quiet #{spec_opts[:url]} #{ref}`

      if $?.exitstatus != 0
        fail "nix-prefetch-git failed with status #{$?.exitstatus}"
      end

      @fetcher_opts = JSON.parse(json.strip, symbolize_names: true)
      spec_opts[:rev] = fetcher_opts[:rev]
      self.channel = nil
    end

    def prefetch_github(override_opts)
      mirror = GitRepoMirror.new(spec_opts[:url])
      mirror.setup

      ref = mirror.revision_parse(override_opts[:ref])
      url = File.join(spec_opts[:url], 'archive', "#{ref}.tar.gz")
      hash = `nix-prefetch-url --unpack "#{url}" 2> /dev/null`

      if $?.exitstatus != 0
        fail "nix-prefetch-url failed with status #{$?.exitstatus}"
      end

      @fetcher_opts = {
        url: url,
        sha256: hash.strip,
      }
      spec_opts[:rev] = ref
    end
  end
end
