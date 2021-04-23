{ confDir, corePkgs, coreLib, findConfig }:
let
  swpinsFor =
    { name, config }:
    import ../swpins/eval.nix {
      inherit confDir name;
      channels = config.swpins.channels;
      pkgs = corePkgs;
      lib = coreLib;
    };

  makeModuleArgs =
    { config, swpins, spin, name }@args: {
      swpins = swpins.evaluated;
      swpinsInfo = swpins.infos;
      confMachine = import ./info.nix (args // { inherit findConfig; });
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
    ++ (import "${toString confDir}/modules/module-list.nix").${spin}
    ++ (import "${toString confDir}/cluster/module-list.nix")
    ++ extraImports;
in rec {
  nixos = { name, config }:
    let
      swpins = swpinsFor { inherit name config; };
    in
      { config, pkgs, ... }@args:
      {
        _module.args = makeModuleArgs {
          inherit config swpins;
          spin = "nixos";
          inherit name;
        };

        imports = makeImports "nixos" [
          "${toString confDir}/cluster/${name}/config.nix"
        ];
      };

  vpsadminos = { name, config }:
    let
      swpins = swpinsFor { inherit name config; };
    in
      { config, pkgs, ... }@args:
      {
        _module.args = makeModuleArgs {
          inherit config swpins;
          spin = "vpsadminos";
          inherit name;
        };

        imports = makeImports "vpsadminos" [
          "${toString confDir}/cluster/${name}/config.nix"
        ];
      };
}
