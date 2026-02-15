{
  confDir,
  inputs,
  system ? null,
}:
let
  resolvedSystem =
    if system != null then
      system
    else if builtins ? currentSystem then
      builtins.currentSystem
    else
      "x86_64-linux";

  pins = import (confDir + "/configs/pins.nix");

  confctlSrc = inputs.confctl;

  coreInputName = pins.core.nixpkgs;
  coreNixpkgs = inputs.${coreInputName};
  corePkgs = import coreNixpkgs { system = resolvedSystem; };
  coreLib = corePkgs.lib;

  confLib = import (confctlSrc + "/nix/lib") {
    inherit confDir;
    coreLib = coreLib;
    corePkgs = corePkgs;
  };

  userClusterModules = confDir + "/modules/cluster/default.nix";

  clusterModules = [
    (
      { ... }:
      {
        _module.args = {
          pkgs = corePkgs;
          inherit confLib;
          swpins = { };
          swpinsInfo = { };
          confMachine = null;
        };
      }
    )
    (confctlSrc + "/nix/modules/cluster")
  ]
  ++ (import (confDir + "/cluster/module-list.nix"))
  ++ (coreLib.optional (builtins.pathExists userClusterModules) userClusterModules);

  evalCluster = coreLib.evalModules {
    modules = clusterModules;
  };

  cluster = evalCluster.config.cluster;

  makeMachine =
    {
      name,
      metaConfig,
      carrier ? null,
      alias ? null,
      clusterName ? null,
    }:
    let
      ensuredClusterName = if clusterName == null then name else clusterName;
    in
    {
      inherit
        name
        alias
        metaConfig
        carrier
        ;
      clusterName = ensuredClusterName;
    };

  generationUpdates =
    cm:
    coreLib.flatten (
      map
        (
          generations:
          map
            (attr: {
              path = [
                generations
                attr
              ];
              update =
                old:
                let
                  v = cm.${generations}.${attr};
                in
                if v == null then old else v;
            })
            [
              "min"
              "max"
              "maxAge"
            ]
        )
        [
          "buildGenerations"
          "hostGenerations"
        ]
    );

  expandCarrier =
    machineAttrs: carrierMachine:
    map (
      cm:
      makeMachine {
        name = "${carrierMachine.name}#${if cm.alias == null then cm.machine else cm.alias}";
        alias = cm.alias;
        clusterName = cm.machine;
        carrier = carrierMachine.name;
        metaConfig = coreLib.updateManyAttrsByPath (
          [
            {
              path = [ "labels" ];
              update = old: old // cm.labels;
            }
            {
              path = [ "tags" ];
              update = old: old ++ cm.tags;
            }
          ]
          ++ (generationUpdates cm)
        ) machineAttrs.${cm.machine}.metaConfig;
      }
    ) carrierMachine.metaConfig.carrier.machines;

  expandCarriers =
    machineAttrs:
    coreLib.flatten (
      coreLib.mapAttrsToList (
        name: m: if m.metaConfig.carrier.enable then [ m ] ++ (expandCarrier machineAttrs m) else m
      ) machineAttrs
    );

  getClusterMachines =
    cluster:
    let
      machineAttrs = coreLib.mapAttrs (
        name: metaConfig: makeMachine { inherit name metaConfig; }
      ) cluster;
    in
    expandCarriers machineAttrs;

  flakeKeyFor =
    name:
    let
      sanitized = builtins.replaceStrings [ "/" "." "-" ":" "#" ] [ "_" "_" "_" "_" "_" ] name;
      base = if builtins.match "^[0-9].*" sanitized != null then "_" + sanitized else sanitized;
      h = builtins.substring 0 8 (builtins.hashString "sha256" name);
    in
    base + "__" + h;

  machines = getClusterMachines cluster;

  machineNames = map (m: m.name) machines;

  machinesAttrs = builtins.listToAttrs (
    map (m: {
      name = m.name;
      value = m // {
        flakeKey = flakeKeyFor m.name;
      };
    }) machines
  );

  lock = builtins.fromJSON (builtins.readFile (confDir + "/flake.lock"));

  swpinInputsFor =
    channels: builtins.foldl' (acc: chan: acc // (pins.channels.${chan} or { })) { } channels;

  swpinPathsFor =
    swpinInputs: coreLib.mapAttrs (_: inputName: inputs.${inputName}.outPath) swpinInputs;

  swpinSpecJsonFor =
    swpinInputs: swpinPaths:
    coreLib.mapAttrs (
      swpinName: inputName:
      let
        node = lock.nodes.${inputName};
        rev = node.locked.rev or null;
        narHash = node.locked.narHash or null;

        derivedUrl =
          if node.original.type == "github" then
            "https://github.com/${node.original.owner}/${node.original.repo}"
          else if node.original ? url then
            let
              raw = node.original.url;
            in
            if coreLib.hasPrefix "git+" raw then coreLib.removePrefix "git+" raw else raw
          else
            "unknown";
      in
      {
        type = "git-rev";
        name = swpinName;
        nix_options = {
          url = derivedUrl;
          fetchSubmodules = false;
          update = {
            auto = false;
            interval = 0;
            ref = null;
          };
        };
        state = {
          rev = rev;
          date = "1970-01-01T00:00:00Z";
        };
        info = {
          rev = rev;
          sha256 = narHash;
        };
        fetcher = {
          type = "directory";
          options = {
            path = swpinPaths.${swpinName};
          };
        };
      }
    ) swpinInputs;

  buildPlan = builtins.listToAttrs (
    map (
      m:
      let
        channels = m.metaConfig.swpins.channels or [ ];
        swpinInputs = swpinInputsFor channels;
        swpinPaths = swpinPathsFor swpinInputs;
      in
      {
        name = m.name;
        value = {
          flakeKey = flakeKeyFor m.name;
          swpinPaths = swpinPaths;
          swpinSpecJson = swpinSpecJsonFor swpinInputs swpinPaths;
        };
      }
    ) machines
  );

  confctlModules = [
    (confctlSrc + "/nix/modules/confctl/generations.nix")
    (confctlSrc + "/nix/modules/confctl/cli.nix")
    (confctlSrc + "/nix/modules/confctl/nix.nix")
    (confctlSrc + "/nix/modules/confctl/overlays.nix")
    (confctlSrc + "/nix/modules/confctl/swpins.nix")
  ];

  confctlConfig = confDir + "/configs/confctl.nix";
  swpinsConfig = confDir + "/configs/swpins.nix";

  evalConfctl = import (coreNixpkgs + "/nixos/lib/eval-config.nix") {
    modules =
      confctlModules
      ++ (coreLib.optional (builtins.pathExists swpinsConfig) swpinsConfig)
      ++ (coreLib.optional (builtins.pathExists confctlConfig) confctlConfig);
    pkgs = corePkgs;
    lib = coreLib;
    system = resolvedSystem;
  };

  settings = evalConfctl.config.confctl;

  baseSystemModule =
    { ... }:
    {
      _module.args = {
        inherit confDir confLib;
        confData = import (confDir + "/data/default.nix") { lib = coreLib; };
        swpins = { };
        swpinsInfo = { };
        confMachine = null;
      };
    };

  systemModulesFor =
    m:
    [
      baseSystemModule
      (confDir + "/cluster/${m.clusterName}/config.nix")
      (confDir + "/environments/base.nix")
    ]
    ++ confctlModules;

  autoRollbackFor =
    pkgs:
    let
      args = {
        src = confctlSrc + "/libexec/auto-rollback.rb";
        isExecutable = true;
      };

      replacements = {
        ruby = pkgs.ruby;
      };
    in
    if pkgs ? replaceVarsWith then
      pkgs.replaceVarsWith {
        inherit (args) src isExecutable;
        inherit replacements;
      }
    else
      pkgs.substituteAll ({ inherit (args) src isExecutable; } // replacements);

  toplevelFor =
    m:
    let
      plan = buildPlan.${m.name};
      swpinPaths = plan.swpinPaths;
      pkgs = import swpinPaths.nixpkgs { system = resolvedSystem; };
      modules = systemModulesFor m;

      evalConfig =
        if m.metaConfig.spin == "nixos" then
          import (swpinPaths.nixpkgs + "/nixos/lib/eval-config.nix") {
            inherit modules pkgs;
            system = resolvedSystem;
          }
        else if m.metaConfig.spin == "vpsadminos" then
          import (swpinPaths.vpsadminos + "/os/default.nix") { inherit modules pkgs; }
        else
          abort "Unsupported spin ${m.metaConfig.spin}";
    in
    evalConfig.config.system.build.toplevel;

  buildOutputs = builtins.listToAttrs (
    map (
      m:
      let
        plan = buildPlan.${m.name};
        swpinPaths = plan.swpinPaths;
        pkgs = import swpinPaths.nixpkgs { system = resolvedSystem; };
      in
      {
        name = plan.flakeKey;
        value = {
          toplevel = toplevelFor m;
          autoRollback = autoRollbackFor pkgs;
        };
      }
    ) machines
  );
in
{
  settings = settings;
  machineNames = machineNames;
  machines = machinesAttrs;
  buildPlan = buildPlan;
  build = buildOutputs;
}
