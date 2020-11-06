{ config, ... }:
let
  nixpkgsBranch = branch: {
    type = "git";

    git = {
      url = "https://github.com/NixOS/nixpkgs";
      update = "refs/heads/${branch}";
    };
  };

  vpsadminosBranch = branch: {
    type = "git-rev";

    git-rev = {
      url = "https://github.com/vpsfreecz/vpsadminos";
      update = "refs/heads/${branch}";
    };
  };
in {
  confctl.swpins.channels = {
    nixos-unstable = { nixpkgs = nixpkgsBranch "nixos-unstable"; };

    # nixos-stable = { nixpkgs = nixpkgsBranch "nixos-20.09"; };

    vpsadminos-master = { vpsadminos = vpsadminosBranch "master"; };
  };
}
