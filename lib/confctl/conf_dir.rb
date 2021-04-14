module ConfCtl
  module ConfDir
    # Path to the directory containing cluster configuration
    # @return [String]
    def self.path
      @path ||= File.realpath(Dir.pwd)
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
  end
end
