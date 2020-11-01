let
  shared = [
    ./cluster
    ./service-definitions.nix
  ];

  nixos = [
    ./services/netboot.nix
  ];

  vpsadminos = [
    ./cluster/configs/node.nix
  ];
in {
  nixos = shared ++ nixos;
  vpsadminos = shared ++ vpsadminos;
}
