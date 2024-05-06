{ confDir, corePkgs, coreLib, findMetaConfig }:
let
  swpinsFor =
    { name, metaConfig }:
    import ../swpins/eval.nix {
      inherit confDir name;
      channels = metaConfig.swpins.channels;
      pkgs = corePkgs;
      lib = coreLib;
    };

  makeModuleArgs =
    { metaConfig, swpins, spin, name }@args: {
      swpins = swpins.evaluated;
      swpinsInfo = swpins.infos;
      confMachine = import ./info.nix (args // { inherit findMetaConfig; });
    };

  makeImports = spin: extraImports: [
    ({ config, pkgs, lib, confMachine, ... }:
    {
      _file = "confctl/nix/lib/machine/default.nix";

      _module.args = {
        inherit confDir;
        confLib = import ../../lib { inherit confDir coreLib corePkgs; };
        confData = import "${toString confDir}/data/default.nix" { inherit lib; };
      };

      networking.hostName =
        lib.mkIf (confMachine.host != null && confMachine.host.name != null) (lib.mkDefault confMachine.host.name);

      networking.domain =
        lib.mkIf (confMachine.host != null) (lib.mkDefault confMachine.host.fullDomain);
    })
  ] ++ (import ../../modules/module-list.nix).${spin}
    ++ (import ../../modules/system-list.nix).${spin}
    ++ (import "${toString confDir}/modules/module-list.nix").${spin}
    ++ (import "${toString confDir}/cluster/module-list.nix")
    ++ extraImports;
in rec {
  nixos = { name, metaConfig }:
    let
      swpins = swpinsFor { inherit name metaConfig; };
    in
      { config, pkgs, ... }@args:
      {
        _module.args = makeModuleArgs {
          metaConfig = config;
          inherit swpins;
          spin = "nixos";
          inherit name;
        };

        imports = makeImports "nixos" [
          "${toString confDir}/cluster/${name}/config.nix"
        ];
      };

  vpsadminos = { name, metaConfig }:
    let
      swpins = swpinsFor { inherit name metaConfig; };
    in
      { config, pkgs, ... }@args:
      {
        _module.args = makeModuleArgs {
          metaConfig = config;
          inherit swpins;
          spin = "vpsadminos";
          inherit name;
        };

        imports = makeImports "vpsadminos" [
          "${toString confDir}/cluster/${name}/config.nix"
        ];
      };
}
