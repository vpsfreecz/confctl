{ config, lib, pkgs, confMachine, ... }:
let
  inherit (lib) concatStringsSep flip mkOption mkEnableOption mkIf
                optional optionalString types;

  concatNl = concatStringsSep "\n";

  cfg = config.confctl.carrier.netboot;

  isoImage =
    { config, ... }:
    {
      options = {
        file = mkOption {
          type = types.path;
          description = ''
            Path to the ISO image file

            If it is a path, the file must be available on the build machine,
            it will be copied into the Nix store and deployed to the target machine.
            If it is a string, then the file must be available on the target machine
            at the given path.
          '';
        };

        label = mkOption {
          type = types.str;
          default = "";
          description = "Menu label for this image";
        };
      };
    };

  baseDir = "/var/lib/confctl/carrier/netboot";

  tftpRoot = "${baseDir}/tftp";

  httpRoot = "${baseDir}/http";

  builderConfig = builtins.toJSON {
    ruby = pkgs.ruby;
    coreutils = pkgs.coreutils;
    syslinux = pkgs.syslinux;
    inherit tftpRoot httpRoot;
    hostName = config.networking.hostName;
    httpUrl = "http://${cfg.host}";
    memtest =
      if cfg.memtest86.enable then
        { package = pkgs.memtest86plus; params = cfg.memtest86.params; }
      else
        null;
    isoImages = cfg.isoImages;
  };

  builder = pkgs.confReplaceVarsWith {
    src = ./build-netboot-server.rb;
    name = "build-netboot-server";
    dir = "bin";
    isExecutable = true;
    replacements = {
      ruby = pkgs.ruby;
      jsonConfig = pkgs.writeText "netboot-server.json" builderConfig;
    };
  };
in {
  options = {
    confctl.carrier.netboot = {
      enable = mkEnableOption ''
        Enable netboot server generated from confctl carrier
      '';

      host = mkOption {
        type = types.str;
        description = "Hostname or IP address of the netboot server";
      };

      enableACME = mkOption {
        type = types.bool;
        description = "Enable ACME and SSL for netboot host";
        default = false;
      };

      memtest86 = {
        enable = mkOption {
          type = types.bool;
          description = "Include memtest in boot menu";
          default = true;
        };

        params = mkOption {
          type = types.listOf types.str;
          default = [];
          example = [ "console=ttyS0,115200" ];
          description = "See {option}`boot.loader.grub.memtest86.params`";
        };
      };

      isoImages = mkOption {
        type = types.listOf (types.submodule isoImage);
        default = [];
        description = "A list of ISO images to be included in boot menu";
      };

      allowedIPv4Ranges = mkOption {
        type = types.listOf types.str;
        description = ''
          Allow HTTP access for these IP ranges, if not specified
          access is not restricted.
        '';
        default = [];
        example = "10.0.0.0/24";
      };

      tftp.bindAddress = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = ''
          The address for the TFTP server to bind on
        '';
      };
    };
  };

  config = mkIf cfg.enable {
    confctl.carrier.onChangeCommands = ''
      ${builder}/bin/build-netboot-server
      rc=$?

      if [ $rc != 0 ] ; then
        echo "build-netboot-server failed with $rc"
        exit 1
      fi
    '';

    environment.systemPackages = [ builder ];

    networking.firewall = {
      extraCommands = mkIf (cfg.allowedIPv4Ranges != []) (concatNl (map (net: ''
        # Allow access from ${net} for netboot
        iptables -A nixos-fw -p udp -s ${net} ${optionalString (!isNull cfg.tftp.bindAddress) "-d ${cfg.tftp.bindAddress}"} --dport 68 -j nixos-fw-accept
        iptables -A nixos-fw -p udp -s ${net} ${optionalString (!isNull cfg.tftp.bindAddress) "-d ${cfg.tftp.bindAddress}"} --dport 69 -j nixos-fw-accept
        iptables -A nixos-fw -p tcp -s ${net} --dport 80 -j nixos-fw-accept
        ${optionalString cfg.enableACME "iptables -A nixos-fw -p tcp -s ${net} --dport 443 -j nixos-fw-accept"}
      '') cfg.allowedIPv4Ranges));
    };

    systemd.services.netboot-atftpd = {
      description = "TFTP Server for Netboot";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      # runs as nobody
      serviceConfig.ExecStart = toString ([
        "${pkgs.atftp}/sbin/atftpd"
        "--daemon"
        "--no-fork"
      ] ++ (optional (!isNull cfg.tftp.bindAddress) [ "--bind-address" cfg.tftp.bindAddress ])
        ++ [ tftpRoot ]);
    };

    services.nginx = {
      enable = true;

      appendConfig = ''
        worker_processes auto;
      '';

      virtualHosts = {
        "${cfg.host}" = {
          root = httpRoot;
          addSSL = cfg.enableACME;
          enableACME = cfg.enableACME;
          locations = {
            "/" = {
              extraConfig = ''
                autoindex on;
                ${optionalString (cfg.allowedIPv4Ranges != []) ''
                  ${concatNl (flip map cfg.allowedIPv4Ranges (range: "allow ${range};"))}
                  deny all;
                ''}
              '';
            };
          };
        };
      };
    };
  };
}