{
  confDir,
  inputs,
  channels ? { },
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

  confctlSrc = inputs.confctl;

  coreNixpkgs = inputs.nixpkgs;
  corePkgs = import coreNixpkgs { system = resolvedSystem; };
  coreLib = corePkgs.lib;

  flakeInputs = coreLib.filterAttrs (n: _: n != "self") inputs;

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
          inherit confLib flakeInputs;
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

  clusterModulesForSystem = [
    (confctlSrc + "/nix/modules/cluster")
  ]
  ++ (import (confDir + "/cluster/module-list.nix"));

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

  mkMachineKey =
    name:
    let
      sanitized =
        let
          chars =
            if builtins ? stringToCharacters then
              builtins.stringToCharacters name
            else
              coreLib.stringToCharacters name;
          safeChar =
            c:
            if (c >= "a" && c <= "z") || (c >= "A" && c <= "Z") || (c >= "0" && c <= "9") || (c == "_") then
              c
            else
              "_";
          s = builtins.concatStringsSep "" (map safeChar chars);
        in
        if builtins.match "^[0-9].*" s != null then "_" + s else s;

      hash = builtins.substring 0 8 (builtins.hashString "sha256" name);
    in
    "m_" + sanitized + "_" + hash;

  machines = getClusterMachines cluster;

  machinesWithKey = map (m: m // { key = mkMachineKey m.name; }) machines;

  machineNames = map (m: m.name) machinesWithKey;

  machineKeys = builtins.listToAttrs (
    map (m: {
      name = m.name;
      value = m.key;
    }) machinesWithKey
  );

  machinesAttrs = builtins.listToAttrs (
    map (m: {
      name = m.name;
      value = m // {
        key = m.key;
        flakeKey = m.key;
      };
    }) machinesWithKey
  );

  machinesByKey = builtins.listToAttrs (
    map (m: {
      name = m.key;
      value = m;
    }) machinesWithKey
  );

  lock = builtins.fromJSON (builtins.readFile (confDir + "/flake.lock"));

  derivedUrlForNode =
    node:
    let
      original = node.original or { };
    in
    if (original.type or null) == "github" then
      "https://github.com/${original.owner}/${original.repo}"
    else if original ? url then
      let
        raw = original.url;
      in
      if coreLib.hasPrefix "git+" raw then coreLib.removePrefix "git+" raw else raw
    else
      null;

  swpinInputsFor =
    channelNames: builtins.foldl' (acc: chan: acc // (channels.${chan} or { })) { } channelNames;

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
          let
            url = derivedUrlForNode node;
          in
          if url == null then "unknown" else url;
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

  swpinInfosFor = swpinSpecJson: coreLib.mapAttrs (_: spec: spec.info or { }) swpinSpecJson;

  mkRoleInfo =
    inputName:
    let
      node = lock.nodes.${inputName} or { };
      src = node.locked or { };
      rev = src.rev or null;
    in
    {
      input = inputName;
      url = derivedUrlForNode node;
      inherit rev;
      shortRev = if rev == null then null else builtins.substring 0 8 rev;
      lastModified = src.lastModified or null;
    };

  pinsInfoFor = swpinInputs: coreLib.mapAttrs (_: inputName: mkRoleInfo inputName) swpinInputs;

  buildPlan = builtins.listToAttrs (
    map (
      m:
      let
        pinsChannels = (m.metaConfig.pins.channels or [ ]);
        swpinsChannels = (m.metaConfig.swpins.channels or [ ]);

        channelNames = if pinsChannels != [ ] then pinsChannels else swpinsChannels;

        pinInputOverrides = (m.metaConfig.pins or { }).inputs or { };
        swpinInputs = (swpinInputsFor channelNames) // pinInputOverrides;
        swpinPaths = swpinPathsFor swpinInputs;
        swpinSpecJson = swpinSpecJsonFor swpinInputs swpinPaths;
        swpinInfos = swpinInfosFor swpinSpecJson;
        pinsInfo = pinsInfoFor swpinInputs;
      in
      {
        name = m.name;
        value = {
          key = m.key;
          flakeKey = m.key;
          swpinPaths = swpinPaths;
          swpinSpecJson = swpinSpecJson;
          swpinInfos = swpinInfos;
          pins = swpinPaths;
          pinsInfo = pinsInfo;
        };
      }
    ) machinesWithKey
  );

  pinsOutput = builtins.listToAttrs (
    map (m: {
      name = m.key;
      value = buildPlan.${m.name}.pins;
    }) machinesWithKey
  );

  pinsInfoOutput = builtins.listToAttrs (
    map (m: {
      name = m.key;
      value = buildPlan.${m.name}.pinsInfo;
    }) machinesWithKey
  );

  confctlModules = [
    (confctlSrc + "/nix/modules/confctl/generations.nix")
    (confctlSrc + "/nix/modules/confctl/cli.nix")
    (confctlSrc + "/nix/modules/confctl/nix.nix")
    (confctlSrc + "/nix/modules/confctl/swpins.nix")
    (confctlSrc + "/nix/modules/confctl/pins-info.nix")
  ];

  confctlConfig = confDir + "/configs/confctl.nix";
  swpinsConfig = confDir + "/configs/swpins.nix";
  userModuleListPath = confDir + "/modules/module-list.nix";
  userModuleList =
    if builtins.pathExists userModuleListPath then
      import userModuleListPath
    else
      {
        nixos = [ ];
        vpsadminos = [ ];
      };

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
  confData = import (confDir + "/data/default.nix") { lib = coreLib; };

  baseSystemModule =
    { ... }:
    {
      _module.args = {
        inherit confDir confLib flakeInputs;
        inherit confData;
      };
    };

  confMachineFor =
    m:
    (confLib.findMetaConfig {
      inherit cluster;
      name = m.clusterName;
    })
    // {
      name = m.clusterName;
    };

  machineArgsModule =
    m:
    { ... }:
    let
      plan = buildPlan.${m.name};
    in
    {
      _module.args = {
        swpins = plan.swpinPaths;
        swpinsInfo = plan.swpinInfos;
        pins = plan.pins;
        pinsInfo = plan.pinsInfo;
        confMachine = confMachineFor m;
      };
    };

  specialArgsFor =
    m:
    let
      plan = buildPlan.${m.name};
    in
    {
      inherit
        confDir
        confLib
        flakeInputs
        confData
        ;
      confMachine = confMachineFor m;
      swpins = plan.swpinPaths;
      swpinsInfo = plan.swpinInfos;
      pins = plan.pins;
      pinsInfo = plan.pinsInfo;
    };

  systemModulesFor =
    m:
    let
      userModules = userModuleList.${m.metaConfig.spin} or [ ];
      systemModules = (import (confctlSrc + "/nix/modules/system-list.nix")).${m.metaConfig.spin} or [ ];
    in
    [
      baseSystemModule
      (machineArgsModule m)
    ]
    ++ clusterModulesForSystem
    ++ [
      (confDir + "/cluster/${m.clusterName}/config.nix")
      (confDir + "/environments/base.nix")
    ]
    ++ confctlModules
    ++ systemModules
    ++ userModules;

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
      specialArgs = specialArgsFor m;

      evalConfig =
        if m.metaConfig.spin == "nixos" then
          import (swpinPaths.nixpkgs + "/nixos/lib/eval-config.nix") {
            inherit modules pkgs;
            system = resolvedSystem;
            specialArgs = specialArgs;
          }
        else if m.metaConfig.spin == "vpsadminos" then
          let
            vpsadminosPath = swpinPaths.vpsadminos;
            vpsadminosOverlays = import (vpsadminosPath + "/os/overlays");
            vpsadminOverlays = import (swpinPaths.vpsadmin + "/nixos/overlays");
            vpsadminosPkgs = import swpinPaths.nixpkgs {
              system = resolvedSystem;
              overlays = vpsadminosOverlays ++ vpsadminOverlays ++ (import (confDir + "/overlays"));
            };
            vpsadminosSpecialArgs = specialArgs // {
              pkgs = vpsadminosPkgs;
            };
            vpsadminosPkgsModule = {
              _file = vpsadminosPath + "/os/default.nix";
              key = vpsadminosPath + "/os/default.nix";
              config = {
                _module = {
                  check = true;
                };
                nixpkgs.system = vpsadminosPkgs.lib.mkDefault resolvedSystem;
              };
            };
            vpsadminosBaseModules = import (vpsadminosPath + "/os/modules/module-list.nix") {
              nixpkgsPath = swpinPaths.nixpkgs;
            };
          in
          vpsadminosPkgs.lib.evalModules {
            prefix = [ ];
            modules = vpsadminosBaseModules ++ [ vpsadminosPkgsModule ] ++ modules;
            specialArgs = vpsadminosSpecialArgs;
          }
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
        name = plan.key;
        value = {
          toplevel = toplevelFor m;
          autoRollback = autoRollbackFor pkgs;
        };
      }
    ) machinesWithKey
  );

  toplevelOutput = coreLib.mapAttrs (_: v: v.toplevel) buildOutputs;
in
{
  settings = settings;
  channels = channels;
  machineNames = machineNames;
  machineKeys = machineKeys;
  machines = machinesAttrs;
  buildPlan = buildPlan;
  pins = pinsOutput;
  pinsInfo = pinsInfoOutput;
  build = buildOutputs;
  toplevel = toplevelOutput;
  lib = {
    mkMachineKey = mkMachineKey;
  };
}
