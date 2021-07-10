{ config, lib, confLib, ... }@args:
with lib;
let
  machine =
    { config, ...}:
    {
      options = {
        managed = mkOption {
          type = types.nullOr types.bool;
          default = null;
          apply = v:
            if !isNull v then v
            else if elem config.spin [ "nixos" "vpsadminos" ] then true
            else false;
          description = ''
            Determines whether the machine is managed using confctl or not

            By default, NixOS and vpsAdminOS machines are managed by confctl.
          '';
        };

        spin = mkOption {
          type = types.enum [ "openvz" "nixos" "vpsadminos" "other" ];
          description = "OS type";
        };

        swpins = {
          channels = mkOption {
            type = types.listOf types.str;
            default = [];
            description = ''
              List of channels from <option>confctl.swpins.channels</option>
              to use on this machine
            '';
          };

          pins = mkOption {
            type = types.attrsOf (types.submodule swpinOptions.specModule);
            default = {};
            description = ''
              List of swpins for this machine, which can supplement or
              override swpins from configured channels
            '';
          };
        };

        addresses = mkOption {
          type = types.nullOr (types.submodule addresses);
          default = null;
          description = ''
            IP addresses
          '';
        };

        netboot = {
          enable = mkEnableOption "Include this system on pxe servers";
          macs = mkOption {
            type = types.listOf types.str;
            default = [];
            description = ''
              List of MAC addresses for iPXE node auto-detection
            '';
          };
        };

        host = mkOption {
          type = types.nullOr (types.submodule host);
          default = null;
        };

        labels = mkOption {
          type = types.attrs;
          default = {};
          description = ''
            Optional user-defined labels to classify the machine
          '';
        };

        tags = mkOption {
          type = types.listOf types.str;
          default = [];
          description = ''
            Optional user-defined tags to classify the machine
          '';
        };

        nix = {
          nixPath = mkOption {
            type = types.listOf types.str;
            default = [];
            description = ''
              List of extra paths added to environment variable
              <literal>NIX_PATH</literal> for <literal>nix-build</literal>
            '';
          };
        };

        buildGenerations = {
          min = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              The minimum number of build generations to be kept on the build
              machine.
            '';
          };

          max = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              The maximum number of build generations to be kept on the build
              machine.
            '';
          };

          maxAge = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              Delete build generations older than
              <option>cluster.&lt;name&gt;.buildGenerations.maxAge</option>
              seconds from the build machine. Old generations are deleted even
              if <option>cluster.&lt;name&gt;.buildGenerations.max</option> is
              not reached.
            '';
          };
        };

        hostGenerations = {
          min = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              The minimum number of generations to be kept on the machine.
            '';
          };

          max = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              The maximum number of generations to be kept on the machine.
            '';
          };

          maxAge = mkOption {
            type = types.nullOr types.int;
            default = null;
            description = ''
              Delete generations older than
              <option>cluster.&lt;name&gt;.hostGenerations.maxAge</option>
              seconds from the machine. Old generations are deleted even
              if <option>cluster.&lt;name&gt;.hostGenerations.max</option> is
              not reached.
            '';
          };
        };
      };
    };

  swpinOptions = import ../../lib/swpins/options.nix { inherit lib; };

  addresses =
    { config, ... }:
    {
      options = {
        primary = mkOption {
          type = types.nullOr (types.submodule (confLib.mkOptions.addresses 4));
          default =
            if config.v4 != [] then
              head config.v4
            else if config.v6 != [] then
              head config.v6
            else
              null;
          description = ''
            Default address other machines should use to connect to this machine

            Defaults to the first IPv4 address if not set
          '';
        };

        v4 = mkOption {
          type = types.listOf (types.submodule (confLib.mkOptions.addresses 4));
          default = [];
          description = ''
            List of IPv4 addresses this machine responds to
          '';
        };

        v6 = mkOption {
          type = types.listOf (types.submodule (confLib.mkOptions.addresses 6));
          default = [];
          description = ''
            List of IPv6 addresses this machine responds to
          '';
        };
      };
    };

  host =
    { config, ... }:
    {
      options = {
        name = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
           Host name
          '';
        };

        location = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            Host location domain
          '';
        };

        domain = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            Host domain
          '';
        };

        fullDomain = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            Domain including location, i.e. FQDN without host name
          '';
          apply = v:
            if isNull v && !isNull config.domain then
              concatStringsSep "." (
                (optional (!isNull config.location) config.location)
                ++ [ config.domain ]
              )
            else
              v;
        };

        fqdn = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            Host FQDN
          '';
          apply = v:
            if isNull v && !isNull config.name && !isNull config.domain then
              concatStringsSep "." (
                [ config.name ]
                ++ (optional (!isNull config.location) config.location)
                ++ [ config.domain ]
              )
            else
              v;
        };

        target = mkOption {
          type = types.nullOr types.str;
          default = config.fqdn;
          description = ''
            Address/host to which the configuration is deployed to
          '';
        };
      };
    };
in {
  options = {
    cluster = mkOption {
      type = types.attrsOf (types.submodule machine);
      default = {};
    };
  };
}
