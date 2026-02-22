{ lib, config, ... }:
let
  # inputsInfo is provided by the config flake via specialArgs / _module.args
  inputsInfo = config._module.args.inputsInfo or null;

  info = inputsInfo;

  json = if info == null then null else builtins.toJSON info;
in
{
  options.confctl.inputsInfo = lib.mkOption {
    type = lib.types.nullOr lib.types.attrs;
    default = info;
    description = "Flake input metadata written to /etc/confctl/inputs-info.json (provided via module args).";
    readOnly = true;
  };

  config = lib.mkIf (json != null) {
    environment.etc."confctl/inputs-info.json".text = json;
  };
}
