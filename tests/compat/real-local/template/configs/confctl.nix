{ ... }: {
  confctl.list.columns = [
    "name"
    "spin"
    "host.fqdn"
    "labels.site"
  ];
  confctl.nix.impureEval = false;
  confctl.nix.legacyNixPath = false;
}
