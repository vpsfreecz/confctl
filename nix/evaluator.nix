{ jsonArg }:
let
  arg = builtins.fromJSON (builtins.readFile jsonArg);

  nixpkgs = import <nixpkgs> {};
  pkgs = nixpkgs.pkgs;
  lib = nixpkgs.lib;
  
  deployments = import ./deployments.nix { inherit (arg) confDir; inherit pkgs lib; };

  nameValuePairs = builtins.map (d: {
    name = d.fqdn;
    value = {
      inherit (d) managed type spin name location domain fqdn role;
      config = configByType d.config d.type d.spin;
    };
  }) deployments;

  deploymentsAttrs = builtins.listToAttrs nameValuePairs;

  fullDeploymentsAttrs = builtins.listToAttrs (builtins.map (d: {
    name = d.fqdn;
    value = d;
  }) deployments);

  configByType = config: type: spin: rec {
    base = {
      inherit (config) addresses netboot;
    };

    container = base // { inherit (config) container; };

    node = base // (nodeConfigBySpin config spin);

    machine = base;
  }.${type};

  nodeConfigBySpin = config: spin: rec {
    base = {
      inherit (config) node;
    };

    openvz = base // { inherit (config) vzNode; };

    vpsadminos = base // { inherit (config) osNode; };
  }.${spin};

  deploymentSwpins = d:
    import ./lib/swpins.nix {
      inherit (arg) confDir;
      name = d.fqdn;
      pkgs = nixpkgs.pkgs;
      lib = lib;
    };

  selectedSwpinsAttrs = builtins.listToAttrs (builtins.map (host: {
    name = host;
    value = deploymentSwpins deploymentsAttrs.${host};
  }) arg.deployments);

  selectedToplevels = builtins.listToAttrs (builtins.map (host: {
    name = host;
    value = buildToplevel fullDeploymentsAttrs.${host};
  }) arg.deployments);

  buildToplevel = d: (evalDeployment d).config.system.build.toplevel;

  evalDeployment = d:
    let
      importPath = {
        nixos = <nixpkgs/nixos/lib/eval-config.nix>;
        vpsadminos = <vpsadminos/os/default.nix>;
      };

      evalConfig = import importPath.${d.spin} {
        modules = [
          ({config, lib, pkgs, ...}: {
            key = "confctl-deploy";
            networking.hostName = lib.mkDefault d.fqdn;
          })
          d.build.toplevel
        ];
      };
    in evalConfig;

  build = {
    # List of deployment hosts
    list = { deployments = builtins.map (d: d.fqdn) deployments; };

    # List of deployments in an attrset: host => config
    info = deploymentsAttrs;

    # JSON file with swpins for selected deployments
    swpins = pkgs.writeText "swpins.json" (builtins.toJSON selectedSwpinsAttrs);

    # JSON file with system.build.toplevel for selected deployments, this must
    # be run with proper NIX_PATH with swpins
    toplevel = pkgs.writeText "toplevels.json" (builtins.toJSON selectedToplevels);
  };
in build.${arg.build}
