{ config
, pkgs
, lib
, confDir
, confLib
, confData
, swpinsName ? "images"
, nixosModules ? [] }:
with lib;
let
  swpins = import ./swpins.nix { name = swpinsName; inherit confDir pkgs lib; };

  deployments = confLib.getClusterDeployments config.cluster;

  deploymentAttrs = listToAttrs (map (d: nameValuePair d.config.host.fqdn d) deployments);

  netbootable = filterAttrs (k: v: v.config.netboot.enable) deploymentAttrs;

  filterDeployments = filter: filterAttrs (k: v: filter v) netbootable;

  filterNodes = filter: filterDeployments (v: !isNull v.config.node && (filter v));

  selectNodes = filter: mapAttrs (k: v: nodeImage v) (filterNodes filter);

  # allows to build vpsadminos with specific
  vpsadminosCustom = {modules ? [], vpsadminos, nixpkgs, vpsadmin}:
    let
      # this is fed into scopedImport so vpsadminos sees correct <nixpkgs> everywhere
      overrides = {
        __nixPath = [ { prefix = "nixpkgs"; path = nixpkgs; } ] ++ builtins.nixPath;
        import = fn: scopedImport overrides fn;
        scopedImport = attrs: fn: scopedImport (overrides // attrs) fn;
        builtins = builtins // overrides;
      };
    in
      builtins.scopedImport overrides (vpsadminos + "/os/") {
        pkgs = nixpkgs;
        system = "x86_64-linux";
        configuration = {};
        modules = modules;
        inherit vpsadmin;
      };

  vpsadminos = {modules ? [], ...}@args: vpsadminosCustom {
    inherit modules;
    vpsadminos = args.vpsadminos or swpins.vpsadminos;
    nixpkgs = args.nixpkgs or swpins.nixpkgs;
    vpsadmin = args.vpsadmin or null;
  };

  vpsadminosBuild = args: (vpsadminos args).config.system.build;

  nodeImage = node:
    let
      nodepins = import ./swpins.nix { name = node.name; inherit confDir pkgs lib; };
      build = vpsadminosBuild {
        modules = [
          {
            imports = [
              node.build.toplevel
            ];
          }
        ];
        inherit (nodepins) vpsadminos nixpkgs vpsadmin;
      };
    in {
      toplevel = build.toplevel;
      kernelParams = build.kernelParams;
      dir = pkgs.symlinkJoin {
        name = "vpsadminos_netboot";
        paths = with build; [ dist ];
      };
      macs = node.config.netboot.macs or [];
    };

  nixosBuild = {modules ? []}:
    (import ("${swpins.nixpkgs}/nixos/lib/eval-config.nix") {
      system = "x86_64-linux";
      modules = [
        ("${swpins.nixpkgs}/nixos/modules/installer/netboot/netboot-minimal.nix")
        ({ config, pkgs, lib, ... }:
        {
          _module.args = {
            inherit confLib confData;
          };
        })
      ] ++ modules;
    }).config.system.build;

  nixosNetboot = {modules ? []}:
    let
      build = nixosBuild { inherit modules; };
    in {
      toplevel = build.toplevel;
      dir = pkgs.symlinkJoin {
        name = "nixos_netboot";
        paths = with build; [ netbootRamdisk kernel netbootIpxeScript ];
      };
    };

  inMenu = name: netbootitem: netbootitem // { menu = name; };

in rec {
  vpsadminosISO =
    let
      build = vpsadminosBuild {
        modules = [{
          imports = [
            "${swpins.vpsadminos}/os/configs/iso.nix"
          ];

          system.secretsDir = null;
        }];
      };
    in build.isoImage;

  # stock NixOS
  nixos = nixosNetboot { };
  nixosZfs = nixosNetboot {
    modules = [ {
        boot.supportedFilesystems = [ "zfs" ];
      } ];
  };

  nixosZfsSSH = nixosNetboot {
    modules = [ {
        imports = nixosModules;
        boot.supportedFilesystems = [ "zfs" ];
        # enable ssh
        systemd.services.sshd.wantedBy = lib.mkForce [ "multi-user.target" ];
      } ];
  };

  nixosItems = {
    nixos = inMenu "NixOS" nixos;
    nixoszfs = inMenu "NixOS ZFS" nixosZfs;
    nixoszfsssh = inMenu "NixOS ZFS SSH" nixosZfsSSH;
  };

  nodesInLocation = domain: location:
    selectNodes (node: node.config.host.domain == domain && node.config.host.location == location);

  allNodes = domain: selectNodes (node: node.config.host.domain == domain);
}
