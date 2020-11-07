{ confDir, pkgs, lib, findConfig }:
let
  swpinsFor =
    { name, config }:
    import ../swpins/eval.nix {
      inherit confDir name pkgs lib;
      channels = config.swpins.channels;
    };

  makeModuleArgs =
    { config, swpins, spin, name }@args: {
      inherit swpins;
      deploymentInfo = import ./info.nix (args // { inherit lib findConfig; });
    };

  makeImports = spin: extraImports: [
    ({ config, pkgs, lib, deploymentInfo, ... }:
    {
      _file = "confctl/nix/lib/deployment/default.nix";

      _module.args = {
        inherit confDir;
        confLib = import ../../lib { inherit confDir lib pkgs; };
        confData = import "${toString confDir}/data/default.nix" { inherit lib; };
      };

      networking.hostName =
        lib.mkIf (deploymentInfo.host != null && deploymentInfo.host.name != null) (lib.mkDefault deploymentInfo.host.name);

      networking.domain =
        lib.mkIf (deploymentInfo.host != null) (lib.mkDefault deploymentInfo.host.fullDomain);
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
      let
        moduleArgs = makeModuleArgs {
          inherit config swpins;
          spin = "vpsadminos";
          inherit name;
        };
      in {
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
