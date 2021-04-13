{ jsonArg }:
let
  arg = builtins.fromJSON (builtins.readFile jsonArg);

  hasCorePkgs = (builtins.hasAttr "coreSwpins" arg) && (builtins.hasAttr "nixpkgs" arg.coreSwpins);

  nixpkgs =
    if hasCorePkgs then
      import arg.coreSwpins.nixpkgs {}
    else
      import <nixpkgs> {};

  deployments = import ./deployments.nix {
    inherit (arg) confDir;
    inherit corePkgs coreLib;
  };

  nameValuePairs = builtins.map (d: {
    name = d.name;
    value = {
      inherit (d) name;
      inherit (d.config) managed spin swpins host addresses netboot labels tags;
      inherit (d.config) nix buildGenerations hostGenerations;
      inherit (d.config) container node osNode vzNode;
    };
  }) deployments;

  deploymentsAttrs = builtins.listToAttrs nameValuePairs;

  fullDeploymentsAttrs = builtins.listToAttrs (builtins.map (d: {
    name = d.name;
    value = d;
  }) deployments);

  coreSwpins =
    import ./lib/swpins/eval.nix {
      inherit (arg) confDir;
      name = "core";
      dir = "";
      channels = evalConfctl.config.confctl.swpins.core.channels;
      pkgs = nixpkgs.pkgs;
      lib = nixpkgs.lib;
    };

  corePkgs =
    if hasCorePkgs then
      nixpkgs
    else if builtins.hasAttr "nixpkgs" coreSwpins.evaluated then
      import coreSwpins.evaluated.nixpkgs {}
    else
      abort "Core swpins not set, run `confctl swpins core update`";

  coreLib = corePkgs.lib;

  deploymentSwpins = d:
    import ./lib/swpins/eval.nix {
      inherit (arg) confDir;
      name = d.name;
      channels = d.config.swpins.channels;
      pkgs = corePkgs.pkgs;
      lib = corePkgs.lib;
    };

  coreSwpinsAttrs = coreSwpins.evaluated;

  selectedSwpinsAttrs = builtins.listToAttrs (builtins.map (host: {
    name = host;
    value = (deploymentSwpins fullDeploymentsAttrs.${host}).evaluated;
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
        ./modules/confctl/generations.nix
        ./modules/confctl/cli.nix
        ./modules/confctl/nix.nix
        ./modules/confctl/swpins.nix
        "${toString arg.confDir}/configs/swpins.nix"
      ] ++ nixpkgs.lib.optional (builtins.pathExists cfg) cfg;
    };

  build = {
    # confctl settings
    confctl = { confctl = evalConfctl.config.confctl; };

    # List of deployment hosts
    list = { deployments = builtins.map (d: d.name) deployments; };

    # List of deployments in an attrset: host => config
    info = deploymentsAttrs;

    # Nix configuration of swpins channels
    listSwpinsChannels = evalConfctl.config.confctl.swpins.channels;

    # JSON file with core swpins
    evalCoreSwpins = corePkgs.writeText "swpins.json" (builtins.toJSON coreSwpinsAttrs);

    # JSON file with swpins for selected deployments
    evalHostSwpins = corePkgs.writeText "swpins.json" (builtins.toJSON selectedSwpinsAttrs);

    # JSON file with system.build.toplevel for selected deployments, this must
    # be run with proper NIX_PATH with swpins
    toplevel = corePkgs.writeText "toplevels.json" (builtins.toJSON selectedToplevels);
  };
in build.${arg.build}
