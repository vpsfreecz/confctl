# confctl carriers
Carrier is a machine that can carry other machines. The concept was created
for the purpose of building netboot servers, but can be used in other settings
as well.

All machines must be defined within the cluster as usual. We can then designate
a machine that will serve as a carrier and provide a list of machines that it
will carry, e.g.:

```nix
# File cluster/pxe-server/module.nix
cluster.pxe-server = {
  # ...
  carrier = {
    enable = true;

    # A list of machines found in the cluster/ directory that will be
    # available on the netboot server. Note that you will have to create
    # your own buildAttribute, so that the resulting path contains bzImage,
    # initrd and possibly a machine.json file.
    machines = [
      {
        machine = "node1";
        buildAttribute = [ "system" "build" "dist" ];
      }
    ];
  };
};
```

Now the `node1` machine is available as two machines within the cluster:
`node1` and `pxe-server#node1`. We can build and deploy both:

  * `confctl deploy node1` will deploy the target machine as defined by the configuration
  * `confctl deploy pxe-server#node1` will build the machine and copy the result to `pxe-server`, its carrier

We need both commands to build a slightly different output, but based on the same
configuration. When deploying `node1`, confctl will build attribute
`config.system.build.toplevel`, where as deploying `pxe-server#node1` will build
attribute `config.system.build.dist`. This attribute is configured in `buildAttribute`
option in the example above. `config.system.build.dist` is defined within vpsAdminOS,
its output is a directory with kernel bzImage, initrd and the root filesystem
in a squashfs image, i.e. what we need for booting from network.

Custom build attributes can be created by the user. For example, this is how
`config.system.build.dist` would be defined for NixOS:

```nix
# File cluster/nixos/config.nix
{ config, pkgs, lib, confMachine, swpinsInfo, ... }:
let
  # machine.json contains metadata about the machine that the carrier uses
  # to assemble the netboot server
  machineJson = pkgs.writeText "machine-${config.networking.hostName}.json" (builtins.toJSON {
    # machine spin, nixos/vpsadminos
    spin = "nixos";

    # fully quantified domain name
    fqdn = confMachine.host.fqdn;

    # label used e.g. in user menus
    label = confMachine.host.fqdn;

    # path to the top-level derivation, needed for system boot
    toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;

    # MAC addresses for auto-detection
    macs = confMachine.netboot.macs;

    # Information used by confctl status
    swpins-info = swpinsInfo;
  });
in {
  imports = [
    <nixpkgs/nixos/modules/installer/netboot/netboot-minimal.nix>
  ];

  # Define custom build attribute
  system.build.dist = pkgs.symlinkJoin {
    name = "nixos-netboot";
    paths = [
      config.system.build.netbootRamdisk
      config.system.build.kernel
      config.system.build.netbootIpxeScript
    ];

    # Install machine.json
    postBuild = ''
      ln -s ${machineJson} $out/machine.json
    '';
  };

  # other NixOS configuration
}
```

The carrier must be configured to handle the carried machines. Netboot server
support is integrated within confctl and it only has to be enabled.

```nix
# File cluster/pxe-server/config.nix
{ config, ... }:
{
  confctl.carrier.netboot = {
    enable = true;

    # IP address or hostname the netboot server will be available on
    host = "192.168.100.5";

    # IP ranges that will have access to the server
    allowedIPRanges = [
      "192.168.100.0/24"
    ];
  };
}
```

The netboot server is rebuilt whenever a machine is deployed to it.
The rebuild can be also run manually using command `build-netboot-server`.

## Other uses
You can define your own commands to be run on the carrier machine when
images are deployed to it:

```nix
# File cluster/pxe-server/config.nix
{ config, ... }:
{
  confctl.carrier.onChangeCommands = ''
    # List profiles of carried machines
    ls -l /nix/var/nix/profiles/confctl-*
  '';
}
```
