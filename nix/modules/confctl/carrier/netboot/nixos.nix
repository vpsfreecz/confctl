{ config, lib, pkgs, confMachine, ... }:
let
  inherit (lib) concatStringsSep flip mkOption mkEnableOption mkIf
                optional optionalString types;

  concatNl = concatStringsSep "\n";

  cfg = config.confctl.carrier.netboot;

  baseDir = "/var/lib/confctl/carrier/netboot";

  tftpRoot = "${baseDir}/tftp";

  httpRoot = "${baseDir}/http";

  builder = pkgs.substituteAll {
    src = ./build-netboot-server.rb;
    name = "build-netboot-server";
    dir = "bin";
    isExecutable = true;
    ruby = pkgs.ruby;
    coreutils = pkgs.coreutils;
    syslinux = pkgs.syslinux;
    inherit tftpRoot httpRoot;
    hostName = config.networking.hostName;
    httpUrl = "http://${cfg.host}";
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

      allowedIPRanges = mkOption {
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
      extraCommands = mkIf (cfg.allowedIPRanges != []) (concatNl (map (net: ''
        # Allow access from ${net} for netboot
        iptables -A nixos-fw -p udp -s ${net} ${optionalString (!isNull cfg.tftp.bindAddress) "-d ${cfg.tftp.bindAddress}"} --dport 68 -j nixos-fw-accept
        iptables -A nixos-fw -p udp -s ${net} ${optionalString (!isNull cfg.tftp.bindAddress) "-d ${cfg.tftp.bindAddress}"} --dport 69 -j nixos-fw-accept
        iptables -A nixos-fw -p tcp -s ${net} --dport 80 -j nixos-fw-accept
        ${optionalString cfg.enableACME "iptables -A nixos-fw -p tcp -s ${net} --dport 443 -j nixos-fw-accept"}
      '') cfg.allowedIPRanges));
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
                ${optionalString (cfg.allowedIPRanges != []) ''
                  ${concatNl (flip map cfg.allowedIPRanges (range: "allow ${range};"))}
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