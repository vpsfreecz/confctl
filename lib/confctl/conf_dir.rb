require 'digest'

module ConfCtl
  module ConfDir
    # Path to the directory containing cluster configuration
    # @return [String]
    def self.path
      @path ||= File.realpath(Dir.pwd)
    end

    # Unique hash identifying the configuration based on its filesystem path
    # @return [String]
    def self.hash
      @hash ||= Digest::SHA256.hexdigest(path)
    end

    # Shorter prefix of {hash}
    # @return [String]
    def self.short_hash
      @short_hash ||= hash[0..7]
    end

    # Path to configuration-specific cache directory
    # @return [String]
    def self.cache_dir
      @cache_dir ||= File.join(path, '.confctl')
    end

    # Path to directory with build generations
    # @return [String]
    def self.generation_dir
      @generation_dir ||= File.join(cache_dir, 'generations')
    end

    # Path to configuration-specific log directory
    # @return [String]
    def self.log_dir
      @log_dir ||= File.join(cache_dir, 'logs')
    end
  end
end
