require 'require_all'

module ConfCtl
  # Root of confctl repository
  # @return [String]
  def self.root
    File.realpath(File.join(File.dirname(__FILE__), '../'))
  end

  # Path to the directory containing cluster configuration
  # @return [String]
  def self.conf_dir
    File.realpath(Dir.pwd)
  end

  # Path to cache directory
  # @return [String]
  def self.cache_dir
    File.join(conf_dir, '.confctl')
  end

  # Path to directory with build generations
  # @return [String]
  def self.generation_dir
    File.join(cache_dir, 'generations')
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
