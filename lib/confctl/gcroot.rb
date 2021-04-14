require 'confctl/utils/file'
require 'etc'
require 'fileutils'

module ConfCtl
  module GCRoot
    extend Utils::File

    def self.dir
      File.join(
        '/nix/var/nix/gcroots/per-user',
        Etc.getlogin,
        "confctl-#{ConfDir.short_hash}",
      )
    end

    def self.exist?(name)
      File.symlink?(File.join(dir, name))
    end

    def self.add(name, path)
      FileUtils.mkdir_p(dir)
      File.symlink(path, File.join(dir, name))
    end

    def self.remove(name)
      unlink_if_exists(File.join(dir, name))
    end
  end
end
