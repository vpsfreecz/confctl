module ConfCtl
  module Swpins
    def self.core_dir
      File.join(ConfDir.path, 'swpins')
    end

    def self.channel_dir
      File.join(ConfDir.path, 'swpins/channels')
    end

    def self.cluster_dir
      File.join(ConfDir.path, 'swpins/cluster')
    end
  end
end

require_rel 'swpins'
