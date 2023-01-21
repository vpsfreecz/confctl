require 'require_all'

module ConfCtl
  module Generation ; end
  module Utils ; end

  # Root of confctl repository
  # @return [String]
  def self.root
    @root ||= File.realpath(File.join(File.dirname(__FILE__), '../'))
  end

  # Path to global cache directory
  # @return [String]
  def self.cache_dir
    @cache_dir ||= File.join(
      ENV['XDG_CACHE_HOME'] || File.join(ENV['HOME'], '.cache'),
      'confctl'
    )
  end

  # Path to a nix asset
  # @param name [String]
  # @return [String]
  def self.nix_asset(name)
    File.join(root, 'nix', name)
  end

  # Return host name without slashes
  # @return [String]
  def self.safe_host_name(host)
    host.gsub(/\//, ':')
  end
end

require_rel 'confctl/*.rb'
require_rel 'confctl/utils'
require_rel 'confctl/generation'

ConfCtl::UserScripts.load_scripts
