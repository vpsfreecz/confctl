require 'digest'
require 'fileutils'
require 'find'
require 'monitor'
require 'open3'
require 'tmpdir'

module ConfCtl
  class GitRepoMirror
    class Error < StandardError
      attr_reader :cache_key

      def initialize(message, cache_key: nil)
        @cache_key = cache_key
        super(cache_key ? "#{message} (cache #{cache_key})" : message)
      end
    end

    SHA = /\A[0-9a-f]{40}\z/

    attr_reader :url, :name

    # @param url [String]
    # @param quiet [Boolean]
    def initialize(url, quiet: false)
      @url = url
      @quiet = quiet
      @name = Digest::SHA256.hexdigest(url)
      @monitor = Monitor.new
    end

    # Keep setup and subsequent reads in one critical section. The monitor also
    # permits these public operations to take the same lock when called alone.
    def synchronize
      @monitor.synchronize do
        return yield self if @locked

        FileUtils.mkdir_p(mirror_dir)
        File.open("#{mirror_path}.lock", File::RDWR | File::CREAT | File::NOFOLLOW, 0o600) do |lock|
          lock.flock(File::LOCK_EX)
          @locked = true
          begin
            yield self
          ensure
            @locked = false
          end
        end
      end
    rescue Error => e
      raise if e.cache_key

      raise Error.new(e.message, cache_key: name)
    rescue SystemCallError, IOError
      raise Error.new('Unable to access the Git mirror cache', cache_key: name)
    end

    def setup(ref: nil)
      synchronize do
        unless ensure_repository!
          git_repo!('fetch', '--no-auto-maintenance', '--no-show-forced-updates', *quiet_options, 'origin', *[ref].compact)
          validate_repository!(mirror_path)
        end
        self
      end
    end

    def revision_parse(str)
      synchronize { git_repo!('rev-parse', str).strip }
    end

    def compare(old_rev, new_rev)
      unless [old_rev, new_rev].all? { |rev| rev.is_a?(String) && SHA.match?(rev) }
        raise Error, 'Git ancestry comparison requires full immutable commit SHAs'
      end
      return :identical if old_rev == new_rev

      synchronize do
        ensure_repository!
        [old_rev, new_rev].each { |rev| ensure_commit!(rev) }
        validate_repository!(mirror_path)
        next :ahead if ancestor?(old_rev, new_rev)

        # A negative merge-base result is authority only after every reachable
        # commit parent was read successfully, without commit-graph shortcuts.
        git_repo!('rev-list', '--quiet', '--parents', old_rev, new_rev, '--')
        ancestor?(new_rev, old_rev) ? :behind : :diverged
      end
    end

    # @param from_ref [String]
    # @param to_ref [String]
    def log(from_ref, to_ref, opts: [])
      synchronize do
        validate_repository!(mirror_path)
        "git log for #{from_ref}..#{to_ref}\n" +
          git_repo!('log', '--no-decorate', '--left-right', '--cherry-mark', *opts, "#{from_ref}..#{to_ref}").strip
      end
    end

    # @param from_ref [String]
    # @param to_ref [String]
    def diff(from_ref, to_ref, opts: [])
      synchronize do
        validate_repository!(mirror_path)
        "git diff for #{from_ref}..#{to_ref}\n" + git_repo!('diff', *opts, "#{from_ref}..#{to_ref}").strip
      end
    end

    protected

    attr_reader :quiet

    def ensure_repository!
      if File.exist?(mirror_path) || File.symlink?(mirror_path)
        validate_repository!(mirror_path)
        return false
      end

      Dir.mktmpdir(".#{name}-", mirror_dir) do |temporary|
        candidate = File.join(temporary, 'mirror')
        git!('clone', '--mirror', *quiet_options, '--', url, candidate)
        validate_repository!(candidate)
        raise Error, 'Git mirror cache changed during initialization' if File.exist?(mirror_path) || File.symlink?(mirror_path)

        File.rename(candidate, mirror_path)
      end
      true
    end

    def validate_repository!(path)
      unless File.directory?(path) && !File.symlink?(path) &&
             File.directory?(File.join(path, 'objects')) && !File.symlink?(File.join(path, 'objects')) &&
             File.file?(File.join(path, 'config')) && !File.symlink?(File.join(path, 'config'))
        raise Error, 'Invalid Git mirror cache; a self-contained bare repository is required'
      end
      if %w[shallow info/grafts objects/info/alternates objects/info/http-alternates commondir].any? do |entry|
           File.exist?(File.join(path, entry)) || File.symlink?(File.join(path, entry))
         end
        raise Error, 'Git mirror cache has shallow, grafted or external object history'
      end

      Find.find(File.join(path, 'objects')) do |entry|
        raise Error, 'Git mirror cache must use self-contained object storage' if File.symlink?(entry)
      end

      bare = git!('--git-dir', path, 'rev-parse', '--is-bare-repository').strip
      origins = git!('--git-dir', path, 'config', '--local', '--no-includes', '--get-all', 'remote.origin.url').lines.map(&:chomp)
      raise Error, 'Git mirror cache is not bare or has an unexpected origin' unless bare == 'true' && origins == [url]

      unsupported = '^(extensions\.(partialclone|worktreeconfig)|remote\..*\.(promisor|partialclonefilter|vcs))$'
      out, status = git('--git-dir', path, 'config', '--get-regexp', unsupported)
      return if status == 1 && out.empty?

      raise Error, 'Git mirror cache has unsupported partial or repository configuration'
    end

    def ensure_commit!(rev)
      return if commit_type(rev) == :commit

      git_repo!('fetch', '--no-auto-maintenance', '--no-show-forced-updates', '--no-tags', *quiet_options, 'origin', rev)
      raise Error, 'Git mirror does not contain the requested commit after fetching' unless commit_type(rev) == :commit
    end

    def commit_type(rev)
      out = git_repo!('cat-file', '--batch-check=%(objectname) %(objecttype)', input: "#{rev}\n").strip
      return :missing if out == "#{rev} missing"
      return :commit if out == "#{rev} commit"

      raise Error, 'Git ancestry comparison requires exact commit objects'
    end

    def ancestor?(from, to)
      _out, status = git('--git-dir', mirror_path, 'merge-base', '--is-ancestor', from, to)
      return true if status == 0
      return false if status == 1

      raise Error, 'Git failed while checking commit ancestry'
    end

    def git_repo!(*, **)
      git!('--git-dir', mirror_path, *, **)
    end

    def git!(*, **)
      out, status = git(*, **)
      raise Error, 'Git mirror operation failed' unless status == 0

      out
    end

    def git(*, input: '')
      # These settings apply only to this Git child, never to caller hooks or
      # Ruby. Do not let an enclosing repository redirect the cache's history.
      redirected = %w[GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY
                      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_GRAFT_FILE GIT_SHALLOW_FILE GIT_NAMESPACE
                      GIT_REPLACE_REF_BASE GIT_CONFIG]
      env = redirected.to_h { |key| [key, nil] }.merge('GIT_NO_REPLACE_OBJECTS' => '1', 'GIT_NO_LAZY_FETCH' => '1')
      options = ['-c', 'core.commitGraph=false', '-c', 'gc.auto=0', '-c', 'maintenance.auto=false']
      out, _err, status = Open3.capture3(
        env, 'git', '--no-replace-objects', *options, *, stdin_data: input
      )
      [out, status.exitstatus]
    rescue SystemCallError, IOError
      raise Error, 'Unable to run Git for mirror evidence'
    end

    def quiet_options
      quiet ? ['--quiet'] : []
    end

    def mirror_path
      File.join(mirror_dir, name)
    end

    def mirror_dir
      File.join(ConfCtl.cache_dir, 'git-mirrors')
    end
  end
end
