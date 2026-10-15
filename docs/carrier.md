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
{ config, pkgs, lib, confMachine, inputsInfo, ... }:
let
  # machine.json contains metadata about the machine that the carrier uses
  # to assemble the netboot server
  machineJson = pkgs.writeText "machine-${config.networking.hostName}.json" (builtins.toJSON {
    # machine spin, nixos/vpsadminos
    spin = "nixos";

    # fully qualified domain name
    fqdn = confMachine.host.fqdn;

    # label used e.g. in user menus
    label = confMachine.host.fqdn;

    # path to the top-level derivation, needed for system boot
    toplevel = builtins.unsafeDiscardStringContext config.system.build.toplevel;

    # NixOS version and git revision
    version = config.system.nixos.version;
    revision = config.system.nixos.revision;

    # MAC addresses for auto-detection
    macs = confMachine.netboot.macs;

    # Information used by confctl status
    inputs-info = inputsInfo;
  });
in {
  imports = [
    "${pkgs.path}/nixos/modules/installer/netboot/netboot-minimal.nix"
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
    allowedIPv4Ranges = [
      "192.168.100.0/24"
    ];
  };
}
```

The netboot server is rebuilt whenever a machine is deployed to it.
The rebuild can be also run manually using command `build-netboot-server`.

## Netboot files and current generations

`build-netboot-server` reads its packaged configuration before taking the
blocking lock at `/run/confctl/build-netboot-server.lock`. It ignores command
arguments. It reads carried profiles from `/nix/var/nix/profiles`; each matching
`confctl-NAME-ID-link` must contain `machine.json` with a `toplevel` path.
For each machine, the first generation encountered whose resolved path matches
the current `confctl-NAME` profile is marked current. Generations are then listed
in descending numeric order, so current need not be the newest generation.

The HTTP root contains `machines.json`, `FQDN/machine.json`, and
`FQDN/ID/generation.json` with ordered machines, generations, boot-file URL
strings, kernel parameters and vpsAdminOS variants. Boot files are absolute
symlinks; `FQDN/current` is a relative symlink to the current ID. The TFTP root
contains Syslinux assets, menus and boot links. MAC routing preserves address
case. The NixOS menu shows current first, followed by the generation list
without its first entry; when current is not newest, this can repeat the current
generation and omit the newest. A generation's `kernel-params` file takes
precedence over `machine.json` parameters.

## Recovering a failed netboot rebuild

The builder creates both candidate trees before installing the TFTP link,
then the HTTP link. It removes the old TFTP tree, then the old HTTP tree, only
after both installs succeed. A failure retains earlier effects: a failed
second install can leave TFTP serving new content, and a cleanup failure can
leave both new trees serving while old trees remain or have been partly removed.

Cleanup accepts only a real directory owned by the helper's effective user,
with a generated suffix of six lowercase hexadecimal characters in the
configured root's parent. It rechecks the recorded directory and parent
identities, excludes new and live roots, and unlinks symlinks without following
their targets. Handwritten names such as `tftp.old`, missing or replaced roots,
and old targets outside the configured parent cause a cleanup refusal after
installation. A TFTP cleanup refusal also leaves the old HTTP tree untouched.
This assumes a stable trusted parent; it does not protect against a privileged
actor changing mounts or paths.

Before recovery, inspect the actual TFTP and HTTP links and the retained
candidate and old trees. A failed rebuild may already be serving the new
content. Resolve a refused old root only after checking its location, type,
owner and contents. Restore a matching module and helper revision separately
if needed. A rerun creates another suffix and does not recover deleted content.
Reverting software does not undo installed links or deletions.

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

`carrier-env` exits with status 1 when extra operands remain. It prints
corrective usage to stdout and `Too many arguments` to stderr. Parser and
failed-command diagnostics omit Ruby exception frames. Help shows the invoked
program name, and failure messages show the attempted command's arguments.
Version queries, including `--version=PKG`, fail with `PROGRAM: version unknown`
without loading Ruby packages.

The profile command runs before the callback. If the callback fails, earlier
profile changes or generation deletions remain. Reverting the helper or confctl
software does not restore deleted generations.

## kexec from the netboot server
Set `confctl.programs.kexec-netboot.enable = true;` within your netbooted machine
configurations. This will add program `kexec-netboot` to your system path.

This program loads a kernel and initrd from the netboot server for kexec.
It discovers the server from the boot parameters. By default, it selects the
generation marked current, or the first listed generation if none is marked.
Use `kexec-netboot --interactive` to select a machine, generation and variant.
See `kexec-netboot --help` or the
[kexec-netboot manual](../nix/modules/confctl/kexec-netboot/kexec-netboot.8.md)
for the available options.

The loaded kernel can be run either by `kexec-netboot -e` or `kexec -e` directly.

## Input metadata in the netboot index

The netboot server's generation JSON exports `inputs_info` when the carried
`machine.json` contains `inputs-info`. It uses the same role metadata as
`/etc/confctl/inputs-info.json`; systems without it report unknown inputs.
Machine boot paths, versions, revisions, MAC addresses and kernel parameters are
independent of this metadata.
