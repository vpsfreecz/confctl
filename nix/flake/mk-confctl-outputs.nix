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
            {
              path = [ "buildAttribute" ];
              update = _: cm.buildAttribute;
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

  roleInputsForChannels =
    channelNames: builtins.foldl' (acc: chan: acc // (channels.${chan} or { })) { } channelNames;

  inputPathsFor = roleInputs: coreLib.mapAttrs (_: inputName: inputs.${inputName}.outPath) roleInputs;

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

  inputsInfoFor = roleInputs: coreLib.mapAttrs (_: inputName: mkRoleInfo inputName) roleInputs;
  buildPlan = builtins.listToAttrs (
    map (
      m:
      let
        channelNames = (m.metaConfig.inputs.channels or [ ]);
        inputsOverrides = (m.metaConfig.inputs or { }).overrides or { };

        roleInputs = (roleInputsForChannels channelNames) // inputsOverrides;
        inputPaths = inputPathsFor roleInputs;
        inputsInfo = inputsInfoFor roleInputs;
      in
      {
        name = m.name;
        value = {
          key = m.key;
          flakeKey = m.key;
          inputs = inputPaths;
          inputsInfo = inputsInfo;
        };
      }
    ) machinesWithKey
  );

  inputsOutput = builtins.listToAttrs (
    map (m: {
      name = m.key;
      value = buildPlan.${m.name}.inputs;
    }) machinesWithKey
  );

  inputsInfoOutput = builtins.listToAttrs (
    map (m: {
      name = m.key;
      value = buildPlan.${m.name}.inputsInfo;
    }) machinesWithKey
  );

  confctlModules = [
    (confctlSrc + "/nix/modules/confctl/generations.nix")
    (confctlSrc + "/nix/modules/confctl/cli.nix")
    (confctlSrc + "/nix/modules/confctl/nix.nix")
    (confctlSrc + "/nix/modules/confctl/inputs-info.nix")
  ];

  confctlConfig = confDir + "/configs/confctl.nix";
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
    modules = confctlModules ++ (coreLib.optional (builtins.pathExists confctlConfig) confctlConfig);
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
        inputs = plan.inputs;
        inputsInfo = plan.inputsInfo;
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
      inputs = plan.inputs;
      inputsInfo = plan.inputsInfo;
    };

  systemModulesFor =
    m:
    let
      userModules = userModuleList.${m.metaConfig.spin} or [ ];
      systemModules = (import (confctlSrc + "/nix/modules/system-list.nix")).${m.metaConfig.spin} or [ ];
      moduleArgsModules =
        if m.metaConfig.spin == "vpsadminos" then
          [ ]
        else
          [
            baseSystemModule
            (machineArgsModule m)
          ];
    in
    moduleArgsModules
    ++ clusterModulesForSystem
    ++ [
      (confDir + "/cluster/${m.clusterName}/config.nix")
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

  evalConfigFor =
    m:
    let
      plan = buildPlan.${m.name};
      inputPaths = plan.inputs;
      modules = systemModulesFor m;
      specialArgs = specialArgsFor m;
    in
    if m.metaConfig.spin == "nixos" then
      let
        evalConfig = import (inputPaths.nixpkgs + "/nixos/lib/eval-config.nix") {
          inherit modules;
          system = resolvedSystem;
          specialArgs = specialArgs;
        };
      in
      {
        inherit evalConfig;
        pkgs = evalConfig.pkgs;
      }
    else if m.metaConfig.spin == "vpsadminos" then
      let
        vpsadminosPath = inputPaths.vpsadminos;
        evalResult = import (vpsadminosPath + "/os/default.nix") {
          nixpkgsPath = inputPaths.nixpkgs;
          modules = modules;
          extraArgs = specialArgs;
          system = resolvedSystem;
        };
        evalConfig = evalResult.eval;
      in
      {
        evalConfig = evalConfig;
        pkgs = evalResult.pkgs;
      }
    else
      abort "Unsupported spin ${m.metaConfig.spin}";

  buildOutputs = builtins.listToAttrs (
    map (
      m:
      let
        plan = buildPlan.${m.name};
        evalResult = evalConfigFor m;
        evalConfig = evalResult.evalConfig;
        pkgs = evalResult.pkgs;
        buildAttrPath =
          m.metaConfig.buildAttribute or [
            "system"
            "build"
            "toplevel"
          ];
        buildAttr = coreLib.attrByPath buildAttrPath null evalConfig.config;
        buildValue =
          if buildAttr == null then
            abort "Attribute 'config.${coreLib.concatStringsSep "." buildAttrPath}' not found on machine ${m.name}"
          else
            buildAttr;
      in
      {
        name = plan.key;
        value = {
          toplevel = buildValue;
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
  inputs = inputsOutput;
  inputsInfo = inputsInfoOutput;
  build = buildOutputs;
  toplevel = toplevelOutput;
  lib = {
    mkMachineKey = mkMachineKey;
  };
}
