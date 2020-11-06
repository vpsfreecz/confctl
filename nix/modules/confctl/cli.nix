{ config, lib, ... }:
with lib;
{
  options = {
    confctl = {
      list.columns = mkOption {
        type = types.listOf types.str;
        default = [
          "name"
          "spin"
          "host.fqdn"
        ];
        description = ''
          Configure which columns should <literal>confctl ls</literal> show.
          Names correspond to options within <literal>cluster.&lt;name&gt;</literal>
          module.
        '';
      };
    };
  };
}
