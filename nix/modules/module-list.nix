let
  shared = [
    ./cluster
    ./confctl/generations.nix
    ./confctl/cli.nix
    ./confctl/nix.nix
    ./confctl/swpins.nix
  ];

  nixos = [
  ];

  vpsadminos = [
  ];
in {
  nixos = shared ++ nixos;
  vpsadminos = shared ++ vpsadminos;
  all = shared ++ nixos ++ vpsadminos;
}
