# kexec-netboot(8)

## NAME
kexec-netboot - prepare a machine for kexec using a netboot kernel and initrd

## SYNOPSIS
`kexec-netboot` [*options*]

## DESCRIPTION
`kexec-netboot` downloads the kernel and initrd from the netboot server the
machine was booted from and loads them using `kexec`.

It can boot any machine configuration available on the netboot server, with a
selected generation and variant, such as the default or single-user mode.

## OPTIONS
`-h`, `--help`
: Print a short help text and exit.

`-s`, `--server-url` *URL*
: Specify the netboot server URL. By default, detect the URL from `httproot`
  in `/proc/cmdline`.

`-m`, `--machine` *FQDN*
: Select a machine from the netboot server.

`-g`, `--generation` *GENERATION*
: Select a machine generation by its number.

`-v`, `--variant` *VARIANT*
: Select a generation variant by its name.

`-i`, `--interactive`
: Ask the user to select the machine, generation and variant interactively.

`-a`, `--append` *PARAMS*
: Append parameters to the kernel command line.

`-u`, `--unload`
: Unload the current kexec target kernel and exit.

`-e`, `--exec`
: Run the currently loaded kernel.

`-y`, `--no-sync`
: Skip syncing filesystems with `--exec`.

## BUGS
Report bugs to https://github.com/vpsfreecz/confctl/issues.

## ABOUT
`kexec-netboot` is part of [confctl](https://github.com/vpsfreecz/confctl).
