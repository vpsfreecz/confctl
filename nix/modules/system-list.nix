{
  nixos = [
    ./confctl/carrier/base.nix
    ./confctl/carrier/netboot/nixos.nix
  ];

  vpsadminos = [
    ./confctl/kexec-netboot
  ];
}