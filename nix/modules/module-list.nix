let
  shared = [
    ./cluster
    ./confctl/cli.nix
    ./confctl/nix.nix
    ./confctl/swpins.nix
    ./service-definitions.nix
  ];

  nixos = [
    ./services/netboot.nix
  ];

  vpsadminos = [
  ];
in {
  nixos = shared ++ nixos;
  vpsadminos = shared ++ vpsadminos;
}
