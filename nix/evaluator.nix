{ jsonArg }:
let
  arg = builtins.fromJSON (builtins.readFile jsonArg);

  nixpkgs = import <nixpkgs> {};
  pkgs = nixpkgs.pkgs;
  lib = nixpkgs.lib;

  deployments = import ./deployments.nix { inherit (arg) confDir; inherit pkgs lib; };

  nameValuePairs = builtins.map (d: {
    name = d.name;
    value = {
      inherit (d) name;
      inherit (d.config) managed spin host addresses netboot labels tags;
      inherit (d.config) container node osNode vzNode;
    };
  }) deployments;

  deploymentsAttrs = builtins.listToAttrs nameValuePairs;

  fullDeploymentsAttrs = builtins.listToAttrs (builtins.map (d: {
    name = d.name;
    value = d;
  }) deployments);

  deploymentSwpins = d:
    import ./lib/swpins.nix {
      inherit (arg) confDir;
      name = d.name;
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

      evalConfig = import importPath.${d.config.spin} {
        modules = [ d.build.toplevel ];
      };
    in evalConfig;

  evalConfctl =
    let
      cfg = "${toString arg.confDir}/configs/confctl.nix";
    in import <nixpkgs/nixos/lib/eval-config.nix> {
      modules = [
        ./modules/confctl.nix
      ] ++ lib.optional (builtins.pathExists cfg) cfg;
    };

  build = {
    # confctl settings
    confctl = { confctl = evalConfctl.config.confctl; };

    # List of deployment hosts
    list = { deployments = builtins.map (d: d.name) deployments; };

    # List of deployments in an attrset: host => config
    info = deploymentsAttrs;

    # JSON file with swpins for selected deployments
    swpins = pkgs.writeText "swpins.json" (builtins.toJSON selectedSwpinsAttrs);

    # JSON file with system.build.toplevel for selected deployments, this must
    # be run with proper NIX_PATH with swpins
    toplevel = pkgs.writeText "toplevels.json" (builtins.toJSON selectedToplevels);
  };
in build.${arg.build}
