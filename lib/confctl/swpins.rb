module ConfCtl
  module Swpins
    def self.channel_dir
      File.join(ConfCtl.conf_dir, 'swpins/channels')
    end

    def self.cluster_dir
      File.join(ConfCtl.conf_dir, 'swpins/cluster')
    end
  end
end

require_rel 'swpins'
