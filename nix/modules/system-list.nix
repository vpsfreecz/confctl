{
  nixos = [
    ./confctl/carrier/base.nix
    ./confctl/carrier/netboot/nixos.nix
    ./confctl/kexec-netboot
    ./confctl/host.nix
  ];

  vpsadminos = [
    ./confctl/kexec-netboot
    ./confctl/host.nix
  ];
}
