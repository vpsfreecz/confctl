{ lib, config, ... }:
let
  info = config._module.args.configurationInfo or null;
  json = if info == null then null else builtins.toJSON info;
in
{
  options.confctl.configurationInfo = lib.mkOption {
    type = lib.types.nullOr lib.types.attrs;
    default = info;
    description = "Configuration source metadata written to /etc/confctl/configuration-info.json.";
    readOnly = true;
  };

  config = lib.mkIf (json != null) {
    environment.etc."confctl/configuration-info.json".text = json;
  };
}
