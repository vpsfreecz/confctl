require 'digest'
require 'singleton'

module ConfCtl
  class ConfDir
    include Singleton

    class << self
      %i(
        path
        hash
        short_hash
        cache_dir
        generation_dir
        log_dir
        user_script_dir
        changed?
        unchanged?
        state_mtime
        update_state
      ).each do |v|
        define_method(v) do |*args, **kwargs, &block|
          instance.send(v, *args, **kwargs, &block)
        end
      end
    end

    def initialize
      @cache = ConfCache.new(self)
    end

    # Path to the directory containing cluster configuration
    # @return [String]
    def path
      @path ||= File.realpath(Dir.pwd)
    end

    # Unique hash identifying the configuration based on its filesystem path
    # @return [String]
    def hash
      @hash ||= Digest::SHA256.hexdigest(path)
    end

    # Shorter prefix of {hash}
    # @return [String]
    def short_hash
      @short_hash ||= hash[0..7]
    end

    # Path to configuration-specific cache directory
    # @return [String]
    def cache_dir
      @cache_dir ||= File.join(path, '.confctl')
    end

    # Path to directory with build generations
    # @return [String]
    def generation_dir
      @generation_dir ||= File.join(cache_dir, 'generations')
    end

    # Path to configuration-specific log directory
    # @return [String]
    def log_dir
      @log_dir ||= File.join(cache_dir, 'logs')
    end

    def user_script_dir
      @user_script_dir ||= File.join(path, 'scripts')
    end

    def changed?
      !unchanged?
    end

    def unchanged?
      @cache.uptodate?
    end

    def state_mtime
      @cache.mtime
    end

    def update_state
      @cache.update
    end
  end
end
