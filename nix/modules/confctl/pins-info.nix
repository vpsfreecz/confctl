{ lib, config, ... }:
let
  # pinsInfo is provided by the config flake via specialArgs / _module.args
  pinsInfo = config._module.args.pinsInfo or null;

  # In migration, also accept `swpinsInfo` if configs still use that name
  swpinsInfo = config._module.args.swpinsInfo or null;

  info =
    if pinsInfo != null then
      pinsInfo
    else if swpinsInfo != null then
      swpinsInfo
    else
      null;

  json = if info == null then null else builtins.toJSON info;
in
{
  options.confctl.pinsInfo = lib.mkOption {
    type = lib.types.nullOr lib.types.attrs;
    default = info;
    description = "Pins metadata written to /etc/confctl/pins-info.json (provided via module args).";
    readOnly = true;
  };

  config = lib.mkIf (json != null) {
    environment.etc."confctl/pins-info.json".text = json;

    # Compatibility for older tools / configs
    environment.etc."confctl/swpins-info.json".text = json;
  };
}
