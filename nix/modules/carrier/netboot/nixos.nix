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

        To use this module, configure confctl carrier in the netboot server's module.nix,
        e.g.:

        ```
        cluster.pxe-server = {
          # ...
          carrier = {
            enable = true;

            # A list of machines found in the cluster/ directory that will be
            # available on the netboot server. Note that you will have to create
            # your own buildAttribute, so that the resulting path contains bzImage,
            # initrd and possibly a machine.json file. Example of that is below.
            machines = [
              {
                machine = "node1";
                buildAttribute = [ "system" "build" "dist" ];
              }
            ];
          };
        };
        ```

        Then in the netboot server's config.nix file:

        ```
        { config, ... }:
        {
          imports = [
            <confctl/nix/modules/carrier/netboot/nixos.nix>
          ];

          confctl.carrier.netboot = {
            enable = true;
            host = "192.168.100.5"; # IP address of the netboot server
            allowedIPRanges = [
              "192.168.100.0/24" # range from which the netboot server will be accessible
            ];
          };
        }
        ```

        TODO
      '';

      host = mkOption {
        type = types.str;
        description = "Hostname or IP address of the netboot server";
      };

      # TODO: this must be handled by system.build.dist output
      # copyItems = mkOption {
      #   type = types.bool;
      #   default = true;
      #   description = ''
      #     If enabled, kernel/initrd/squashfs images are copied to tftp/nginx
      #     roots, so that dependencies on the contained store paths are dropped.

      #     When deploying to a remote PXE server, you want this option to be enabled
      #     to reduce the amount of data being transfered. If the PXE server
      #     is running on the build machine itself, disabling this option will
      #     make the build faster.
      #   '';
      # };

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