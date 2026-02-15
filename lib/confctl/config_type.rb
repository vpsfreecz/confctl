module ConfCtl
  module ConfigType
    def self.flake?(conf_dir)
      File.exist?(File.join(conf_dir, 'flake.nix'))
    end
  end
end
