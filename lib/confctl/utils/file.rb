module ConfCtl
  module Utils::File
    # Atomically replace or create symlink
    # @param path [String] symlink path
    # @param dst [String] destination
    def replace_symlink(path, dst)
      replacement = "#{path}.new-#{SecureRandom.hex(3)}"
      File.symlink(dst, replacement)
      File.rename(replacement, path)
    end

    # Unlink file if it exists
    # @param path [String]
    def unlink_if_exists(path)
      File.unlink(path)
      true
    rescue Errno::ENOENT
      false
    end
  end
end
