{ jsonArg }:
let
  arg = builtins.fromJSON (builtins.readFile jsonArg);

  hasCorePkgs = (builtins.hasAttr "coreSwpins" arg) && (builtins.hasAttr "nixpkgs" arg.coreSwpins);

  nixpkgs =
    if hasCorePkgs then
      import arg.coreSwpins.nixpkgs {}
    else
      import <nixpkgs> {};

  machines = import ./machines.nix {
    inherit (arg) confDir;
    inherit corePkgs coreLib;
  };

  nameValuePairs = builtins.map (m: {
    name = m.name;
    value = {
      inherit (m) name alias clusterName carrier metaConfig;
    };
  }) machines;

  machinesAttrs = builtins.listToAttrs nameValuePairs;

  fullMachinesAttrs = builtins.listToAttrs (builtins.map (m: {
    name = m.name;
    value = m;
  }) machines);

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

  machineSwpins = m:
    import ./lib/swpins/eval.nix {
      inherit (arg) confDir;
      name = m.name;
      channels = m.metaConfig.swpins.channels;
      pkgs = corePkgs.pkgs;
      lib = corePkgs.lib;
    };

  coreSwpinsAttrs = coreSwpins.evaluated;

  selectedSwpinsAttrs = builtins.listToAttrs (builtins.map (host: {
    name = host;
    value = (machineSwpins fullMachinesAttrs.${host}).evaluated;
  }) arg.machines);

  selectedToplevels = builtins.listToAttrs (builtins.map (host: {
    name = host;
    value = buildToplevel fullMachinesAttrs.${host};
  }) arg.machines);

  buildToplevel = machine:
    let
      machineConfig = (evalMachine machine).config;

      buildAttr = coreLib.attrByPath machine.build.attribute null machineConfig;

      result =
        if isNull buildAttr then
          abort "Attribute 'config.${coreLib.concatStringsSep "." machine.build.attribute}' not found on machine ${machine.name}"
        else
          buildAttr;
    in {
      attribute = result;

      autoRollback =
        # pkgs.substituteAll was removed from nixos-unstable, but pkgs.replaceVarsWith
        # is not available in nixos-25.05.
        let
          args = {
            src = ../libexec/auto-rollback.rb;
            isExecutable = true;
          };

          replacements = {
            ruby = nixpkgs.ruby;
          };
        in
          if builtins.hasAttr "replaceVarsWith" corePkgs then
            corePkgs.replaceVarsWith {
              inherit (args) src isExecutable;
              inherit replacements;
            }
          else
            corePkgs.substituteAll ({
              inherit (args) src isExecutable;
            } // replacements);
    };

  evalMachine = machine:
    let
      importPath = {
        nixos = <nixpkgs/nixos/lib/eval-config.nix>;
        vpsadminos = <vpsadminos/os/default.nix>;
      };

      evalConfig = import importPath.${machine.metaConfig.spin} {
        modules = machine.extraModules ++ [ machine.build.toplevel ];
      };
    in evalConfig;

  evalConfctl =
    let
      cfg = "${toString arg.confDir}/configs/confctl.nix";
    in evalNixosModules ([
      ./modules/confctl/generations.nix
      ./modules/confctl/cli.nix
      ./modules/confctl/nix.nix
      ./modules/confctl/swpins.nix
      "${toString arg.confDir}/configs/swpins.nix"
    ] ++ nixpkgs.lib.optional (builtins.pathExists cfg) cfg);

  docToplevels = [
    "cluster."
    "confctl."
  ];

  filterOption = o:
    !o.internal && builtins.any (top: nixpkgs.lib.hasPrefix top o.name) docToplevels;

  docModules =
    let
      userModules = "${toString arg.confDir}/modules/cluster/default.nix";
    in evalNixosModules (
      (import ./modules/module-list.nix).all
      ++
      (nixpkgs.lib.optional (builtins.pathExists userModules) userModules)
    );

  docOptions =
    builtins.filter filterOption (nixpkgs.lib.optionAttrSetToDocList docModules.options);

  evalNixosModules = modules:
    import <nixpkgs/nixos/lib/eval-config.nix> {
      modules = modules ++ [
        ({ ... }:
        {
          _module.args = {
            confLib = import ./lib {
              confDir = "${toString arg.confDir}";
              coreLib = nixpkgs.lib;
              corePkgs = nixpkgs.pkgs;
            };
          };
        })
      ];
    };

  build = {
    # confctl settings
    confctl = nixpkgs.writeText "confctl-settings.json" (builtins.toJSON { confctl = evalConfctl.config.confctl; });

    # List available nixos module options for documentation purposes
    moduleOptions = docOptions;

    # List of machines
    list = { machines = builtins.map (m: m.name) machines; };

    # List of machines in an attrset: host => config
    info = corePkgs.writeText "machine-list.json" (builtins.toJSON machinesAttrs);

    # Nix configuration of swpins channels
    listSwpinsChannels = evalConfctl.config.confctl.swpins.channels;

    # JSON file with core swpins
    evalCoreSwpins = corePkgs.writeText "swpins.json" (builtins.toJSON coreSwpinsAttrs);

    # JSON file with swpins for selected machines
    evalHostSwpins = corePkgs.writeText "swpins.json" (builtins.toJSON selectedSwpinsAttrs);

    # JSON file with system.build.toplevel for selected machines, this must
    # be run with proper NIX_PATH with swpins
    toplevel = corePkgs.writeText "toplevels.json" (builtins.toJSON selectedToplevels);
  };
in build.${arg.build}
