let
  shared = [
    ./cluster
    ./confctl.nix
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
