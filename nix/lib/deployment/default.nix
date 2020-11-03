{ confDir, pkgs, lib, findConfig }:
let
  swpinsFor = name: import ../swpins.nix { inherit confDir name pkgs lib; };

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
        lib.mkIf (deploymentInfo.host != null) (lib.mkDefault deploymentInfo.host.fqdn);
    })
  ] ++ (import ../../modules/module-list.nix).${spin}
    ++ (import "${toString confDir}/modules/module-list.nix").${spin}
    ++ (import "${toString confDir}/cluster/module-list.nix")
    ++ extraImports;
in rec {
  nixos = { name }:
    let
      swpins = swpinsFor name;
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

  vpsadminos = { name }:
    let
      swpins = swpinsFor name;
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
