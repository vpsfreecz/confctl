{ confDir, pkgs, lib, findConfig }:
let
  swpinsFor = name: import ../swpins.nix { inherit confDir name pkgs lib; };

  makeModuleArgs =
    { config, swpins, type, spin, name, location ? null, domain, fqdn }@args: {
      inherit swpins;
      deploymentInfo = import ./info.nix (args // { inherit lib findConfig; });
    };

  makeImports = spin: extraImports: [
    ({ config, pkgs, lib, ... }:
    {
      _module.args = {
        confLib = import ../../lib { inherit confDir lib pkgs; };
        data = import "${toString confDir}/data/default.nix" { inherit lib; };
      };
    })
  ] ++ (import ../../modules/module-list.nix).${spin}
    ++ (import "${toString confDir}/modules/module-list.nix").${spin}
    ++ (import "${toString confDir}/cluster/module-list.nix")
    ++ extraImports;
in rec {
  nixosMachine = { name, location ? null, domain, fqdn }:
    let
      swpins = swpinsFor fqdn;
    in
      { config, pkgs, ... }@args:
      {
        _module.args = makeModuleArgs {
          inherit config swpins;
          type = "machine";
          spin = "nixos";
          inherit name location domain;
          fqdn = fqdn;
        };

        imports = makeImports "nixos" [
          "${toString confDir}/cluster/${domain}/machines/${lib.optionalString (location != null) location}/${name}/config.nix"
        ];
      };

  osCustom = { type, name, location ? null, domain, fqdn, role ? null, config }:
    let
      swpins = swpinsFor fqdn;
      configFn = config;
    in
      { config, pkgs, ... }@args:
      let
        moduleArgs = makeModuleArgs {
          inherit config swpins type name location domain;
          spin = "vpsadminos";
          fqdn = fqdn;
        };
      in {
        _module.args = moduleArgs;

        imports = makeImports "vpsadminos" [
          (configFn (args // moduleArgs))
        ];
      };

  osNode = { name, location, domain, fqdn, role }:
    osCustom {
      type = "node";
      inherit name location domain fqdn role;
      config =
        { config, pkgs, swpins, ... }:
        {
          imports = [
            "${toString confDir}/cluster/${domain}/nodes/${location}/${name}/config.nix"
          ];

          nixpkgs.overlays = [
            (import "${swpins.vpsadminos}/os/overlays/vpsadmin.nix" swpins.vpsadmin)
          ];
        };
    };

  osMachine = { name, location ? null, domain, fqdn }:
    osCustom {
      type = "machine";
      inherit name location domain fqdn;
      config =
        { config, pkgs, ... }:
        {
          imports = [
            "${toString confDir}/cluster/${domain}/machines/${lib.optionalString (location != null) location}/${name}/config.nix"
          ];
        };
    };

  container = { name, location ? null, domain, fqdn }:
    let
      swpins = swpinsFor fqdn;
    in
      { config, pkgs, ... }:
      {
        _module.args = makeModuleArgs {
          inherit config swpins;
          type = "container";
          spin = "nixos";
          inherit name location domain fqdn;
        };

        imports = makeImports "nixos" [
          "${toString confDir}/cluster/${domain}/containers/${lib.optionalString (location != null) location}/${name}/config.nix"
        ];
      };
}
