require 'fileutils'
require 'json'

module ConfCtl
  # Cache the list of configuration files to detect changes
  #
  # The assumption is that if there hasn't been any changes in configuration
  # directory, we can reuse previously built artefacts, such as the list of
  # machines, etc. This is much faster than invoking nix-build to query
  # the machines.
  class ConfCache
    # @param conf_dir [ConfDir]
    def initialize(conf_dir)
      @conf_dir = conf_dir
      @cmd = SystemCommand.new
      @cache_dir = File.join(conf_dir.cache_dir, 'build')
      @cache_file = File.join(@cache_dir, 'git-files.json')
      @files = {}
      @loaded = false
      @uptodate = nil
      @checked_at = nil
    end

    # Load file list from cache file
    def load_cache
      begin
        data = File.read(@cache_file)
      rescue Errno::ENOENT
        return
      end

      @files = JSON.parse(data)['files']
      @loaded = true
    end

    # Check if cached file list differs from files on disk
    # @param build_file [String] path to a build artefact to check against
    def uptodate?(build_file)
      if !@uptodate.nil?
        # We're not uptodate if the cache is newer than the build artefact
        begin
          build_st = File.lstat(build_file)
        rescue Errno::ENOENT
          return false
        end

        begin
          cache_st = File.lstat(@cache_file)
        rescue Errno::ENOENT
          return false
        end

        if build_st.mtime >= cache_st.mtime
          return @uptodate
        else
          return false
        end
      end

      @uptodate = check_uptodate
      @uptodate
    end

    # Update cache file with the current state of the configuration directory
    def update
      @files.clear

      list_files.each do |file|
        begin
          st = File.lstat(file)
        rescue Errno::ENOENT
          next
        end

        @files[file] = {
          'mtime' => st.mtime.to_i,
          'size' => st.size,
        }
      end

      tmp = "#{@cache_file}.new"

      FileUtils.mkpath(@cache_dir)
      File.write(tmp, {'files' => @files}.to_json)
      File.rename(tmp, @cache_file)

      @uptodate = true
    end

    protected
    def check_uptodate
      load_cache unless @loaded
      return false if @files.empty?

      list_files.each do |file_path|
        file = @files[file_path]
        return false if file.nil?

        begin
          st = File.lstat(file_path)
        rescue Errno::ENOENT
          return false
        end

        return false if file['mtime'] != st.mtime.to_i || file['size'] != st.size
      end

      true
    end

    def list_files
      out, _ = @cmd.run('git', '-C', @path, 'ls-files', '-z')
      out.strip.split("\0")
    end
  end
end
