require 'securerandom'

module ConfCtl::Cli
  class Configuration < Command
    DIR_MODE = 0755
    FILE_MODE = 0644

    def init
      dir = File.realpath(Dir.pwd)

      Dir.entries(dir).each do |v|
        if !%w(. .. shell.nix .gems).include?(v)
          fail 'init must be called in an empty directory'
        end
      end

      mkdir('cluster')

      mkfile('cluster/module-list.nix') do |f|
        f.puts(<<END
(import ./cluster.nix) ++ [
  # Place for custom modules
]
END
        )
      end

      mkfile('cluster/cluster.nix') do |f|
        f.puts(<<END
# This file is generated by confctl, changes will be lost
[]
END
        )
      end

      mkdir('data')

      mkfile('data/default.nix') do |f|
        f.puts(<<END
{ lib }:
{
  # Place to load custom data sets
  sshKeys = import ./ssh-keys.nix;
}
END
        )
      end

      mkfile('data/ssh-keys.nix') do |f|
        f.puts(<<END
rec {
  admins = [
    # someone
  ];

  someone = "...ssh public key...";
}
END
        )
      end

      mkdir('environments')

      mkfile('environments/base.nix') do |f|
        f.puts(<<END
{ config, pkgs, confData, ... }:
{
  time.timeZone = "Europe/Amsterdam";

  services.openssh.enable = true;

  environment.systemPackages = with pkgs; [
    vim
    screen
  ];

  users.users.root.openssh.authorizedKeys.keys = with confData.sshKeys; admins;
}
END
        )
      end

      mkdir('modules')
      mkfile('modules/module-list.nix') do |f|
        f.puts(<<END
rec {
  shared = [
    # Modules not dependent on spin
  ];

  nixos = shared ++ [
    # Modules only for NixOS
  ];

  vpsadminos = shared ++ [
    # Modules only for vpsAdminOS
  ];
}
END
        )
      end

      mkdir('swpins')
      mkdir('swpins/channels')
      mkdir('swpins/files')
    end

    def add
      require_args!('name')

      name = args[0]
      dir = File.join('cluster', name)
      depth = name.count('/')

      mkdir(dir)

      mkfile(File.join(dir, 'module.nix')) do |f|
        f.puts(<<END
{ config, ... }:
{
  cluster."#{name}" = {
    spin = "nixos";
    host = { name = "machine"; domain = "example.com"; };
  };
}
END
        )
      end

      mkfile(File.join(dir, 'config.nix')) do |f|
        f.puts(<<END
{ config, pkgs, lib, ... }:
{
  imports = [
    #{'../' * (depth + 2)}environments/base.nix
  ];

  # ... standard NixOS configuration ...

  networking.hostName = "#{name.gsub(/\//, '-')}";
}
END
        )
      end

      rediscover
    end

    def rediscover
      hosts = discover_dir('cluster').sort

      replace_file('cluster/cluster.nix') do |f|
        f.puts("# This file is generated by confctl, changes will be lost")
        f.puts("[")

        hosts.each do |host|
          f.puts("  ./#{host}/module.nix")
        end

        f.puts("]")
      end
    end

    protected
    def discover_dir(dir_path, rel_path = nil)
      ret = []

      Dir.entries(dir_path).each do |v|
        entry_abs_path = File.join(dir_path, v)
        next if %w(. ..).include?(v) || !File.directory?(entry_abs_path)

        entry_rel_path = File.join(*[rel_path, v].compact)

        if File.exist?(File.join(entry_abs_path, 'module.nix')) \
           && File.exist?(File.join(entry_abs_path, 'config.nix'))
          ret << entry_rel_path
        end

        ret.concat(discover_dir(entry_abs_path, entry_rel_path))
      end

      ret
    end

    def mkdir(name)
      puts "mkdir #{name}"
      Dir.mkdir(name, DIR_MODE)
    end

    def mkfile(name)
      puts "mkfile #{name}"
      f = File.open(name, 'w', FILE_MODE)
      yield(f)
      f.close
    end

    def replace_file(name)
      puts "replace #{name}"
      replacement = "#{name}.new-#{SecureRandom.hex(3)}"

      File.open(replacement, 'w', FILE_MODE) do |f|
        yield(f)
      end

      File.rename(replacement, name)
    end
  end
end