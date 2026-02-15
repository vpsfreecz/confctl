{ config, ... }:
{
  confctl = {
    nix = {
      impureEval = true;
    };

    # listColumns = {
    #   "name"
    #   "spin"
    #   "host.fqdn"
    # };
  };
}
