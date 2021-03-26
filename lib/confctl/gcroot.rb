require 'etc'
require 'fileutils'

module ConfCtl
  module GCRoot
    def self.dir
      File.join('/nix/var/nix/gcroots/per-user', Etc.getlogin, 'confctl')
    end

    def self.exist?(name)
      File.symlink?(File.join(dir, name))
    end

    def self.add(name, path)
      FileUtils.mkdir_p(dir)
      File.symlink(path, File.join(dir, name))
    end

    def self.remove(name)
      File.unlink(File.join(dir, name))
    end
  end
end
