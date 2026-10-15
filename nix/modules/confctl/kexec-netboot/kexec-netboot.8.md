---
title: "kexec-netboot"
section: "8"
date: "2026-10-14"
footer: "kexec-netboot"
header: "kexec-netboot"
---

# Name

kexec-netboot - load a kernel and initrd from a netboot server

# Synopsis

**kexec-netboot** [*options*]

# Description

**kexec-netboot** downloads a kernel and initrd from the netboot server used
to boot the machine and loads them with **kexec**. It can select any machine
available on that server, together with a generation and a variant such as
single-user mode.

For downloads, the helper reads the first `httproot=` value in `/proc/cmdline`
to find the server. This parameter is required even with **`--server-url`**. Automatic
discovery preserves HTTP or HTTPS and an explicit port.

# Options

**`-h`**, **`--help`**
:   Print help and exit. Help shows the invoked program name.

**`-s`**, **`--server-url`** *URL*
:   Use this server base URL. The helper appends `machines.json` to it.
    By default, the URL comes from `httproot=` in `/proc/cmdline`.

**`-m`**, **`--machine`** *FQDN*
:   Select a machine from the server. The default is the machine built into
    the helper package.

**`-g`**, **`--generation`** *GENERATION*
:   Select a generation by number. A negative value selects an offset in
    index order: `-1` selects the second entry. Without this option, the
    helper selects the first generation whose `current` value is `true`,
    or the first listed generation if none has that value. It preserves
    the server's order.

**`-v`**, **`--variant`** *VARIANT*
:   Select a variant by name. The default is the first available variant.
    An unknown explicit variant prints a warning and continues without
    a variant.

**`-i`**, **`--interactive`**
:   Select the machine, generation and variant interactively. Press ENTER
    to accept the displayed defaults. At the final confirmation, enter
    `y` or `n`; case and surrounding whitespace are ignored. An empty
    response repeats the prompt. EOF at this prompt fails without loading
    the kernel.

**`-a`**, **`--append`** *PARAMS*
:   Append parameters to the kernel command line. The helper passes the
    appended value as literal data in one kexec argument. It does not
    expand quotes, dollar signs or shell substitutions. It preserves
    the joined parameters, including the trailing space when the
    appended value is empty.

**`-u`**, **`--unload`**
:   Unload the current kexec target kernel and exit.

**`-e`**, **`--exec`**
:   Run the currently loaded kernel.

**`-y`**, **`--no-sync`**
:   Skip filesystem synchronization. Load, exec and unload all pass
    `--no-sync`, including when this option is omitted. The option does
    not enable synchronization.

# Downloads

Downloads use HTTP/1.1. HTTPS verifies the certificate chain and hostname
against system trust. The helper follows the standard `HTTP_PROXY`,
`HTTPS_PROXY` and `NO_PROXY` environment rules, including lowercase forms,
uppercase precedence, loopback bypass and CGI safeguards. HTTP and HTTPS
proxies are supported. Malformed selected proxies and other proxy schemes
fail.

Each request uses a fresh connection. Redirects fail, and the helper does
not retry failed downloads. Connection establishment and the TLS handshake
each have a 60-second limit. Reads and writes refresh their own 60-second
inactivity limits. There is no total download-duration limit or deadline
for the kexec command.

# Temporary files and failures

Kernel and initrd downloads use exclusively created mode-0600 temporary
files in the process temporary directory, respecting `TMPDIR`. When the
invocation returns, the helper closes and attempts to remove every file
it created, including after a failed download or kexec command.

The primary failure takes precedence over cleanup errors. The helper
reports cleanup errors and continues cleaning up the remaining owned
files. After a successful load, an unlink failure prints a warning and
leaves success unchanged.

Cleanup cannot undo a kernel load or cover SIGKILL, power loss or a kernel
transition. Reverting the helper software does not undo a loaded kernel.

Option and dependency errors print a diagnostic. Version queries, including
empty or opaque `--version=PKG` values, return `PROGRAM: version unknown`.

# Bugs

Report bugs at <https://github.com/vpsfreecz/confctl/issues>.

# About

**kexec-netboot** is part of [confctl](https://github.com/vpsfreecz/confctl).
