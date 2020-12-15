require 'digest'
require 'fileutils'

module ConfCtl
  class GitRepoMirror
    attr_reader :url, :name

    # @param url [String]
    def initialize(url)
      @url = url
      @name = Digest::SHA256.hexdigest(url)
    end

    def setup
      begin
        File.stat(mirror_path)
      rescue Errno::ENOENT
        FileUtils.mkdir_p(mirror_path)
        git("clone --mirror \"#{url}\" \"#{mirror_path}\"")
      else
        git_repo('fetch')
      end
    end

    def revision_parse(str)
      git_repo("rev-parse #{str}")
    end

    protected
    def git_repo(cmd)
      git("-C \"#{mirror_path}\" #{cmd}")
    end

    def git(cmd)
      ret = `git #{cmd}`.strip

      if $?.exitstatus != 0
        fail "git #{cmd} failed with exit status #{$?.exitstatus}"
      end

      ret
    end

    def mirror_path
      File.join(mirror_dir, name)
    end

    def mirror_dir
      File.join(ConfCtl.cache_dir, 'git-mirrors')
    end
  end
end
