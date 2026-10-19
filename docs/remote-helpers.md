# Remote helper backends

The four Rust helper entrypoints delegate to concrete implementations in
`src/remote/`. They preserve the existing public command lines and file
protocols. They do not invoke Ruby or Python as a runtime fallback, load
extensions, or dispatch a generic remote task protocol. Test probes and fixture
switch programs under `tests/compat/helpers/qualification/` are separate test
tools and are never installed.

## Package data

The module wrappers supply these fixed private fields, overwriting inherited
values with shell-quoted package data before executing the absolute backend:

| Backend | Private package field | Meaning |
| --- | --- | --- |
| carrier-env | `CONFCTL_INTERNAL_CARRIER_ON_CHANGE` | Absolute generated on-change executable |
| build-netboot-server | `CONFCTL_INTERNAL_NETBOOT_CONFIG` | Absolute existing JSON configuration path |
| kexec-netboot | `CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN` | Required default machine string; empty is valid data |
| kexec-netboot | `CONFCTL_INTERNAL_KEXEC_EXECUTABLE` | Absolute packaged kexec executable |

Paths are required, nonempty and absolute. There is no executable discovery or
PATH fallback for these fields. Parsing help and argument errors precedes
operational package-data lookup for the helpers that have a public parser. The
builder retains its original unused-surplus-arguments behavior and no public
parser. Its JSON has no borrowed kexec FQDN field.

The Rust builder accepts old JSON containing the unused `ruby` key. Producer
migration can first select Rust with old JSON, then omit only that key. Rollback
to the old Ruby builder requires restoring the old wrapper and old JSON
together. Other JSON fields, module options and public argv stay unchanged.
Removing a key alone does not prove a Ruby-free runtime closure.

## Carrier mutation and netboot publication

Carrier mutation invokes `nix-env` with the original ordered profile/action
arguments. It runs the generated on-change executable directly after success.
The callback is not interpreted as a shell command string. A failed profile
operation prevents the callback; callback failure leaves prior profile effects
in place. Empty configured on-change lines still produce a real executable.

The builder takes the original standalone lock at
`/run/confctl/build-netboot-server.lock` while scanning profiles, resolving current
generations, listing machines and publishing. This lock is separate from the
configuration service's `.confctl/mutation.lock`. Profile generations retain
numeric ordering without native integer overflow. HTTP/TFTP JSON, PXE menus,
variants, MAC links, syslinux, memtest and ISO entries follow the retained Ruby
formats, including the original NixOS generation-menu behavior.

Both staging trees are built before either publication. The builder installs
TFTP first, then HTTP, using the packaged `ln -sfn`; it removes old roots only
after both installations succeed. The root pair is not a transaction. A failed
second installation can leave new TFTP and old HTTP, while first-install failure
leaves both staging trees and both old published roots. Failure does not invent
a rollback or staging-tree cleanup.

## Auto rollback

The parent prints the existing banner, validates argv, then privately reexecutes
the same executable. The worker inherits the process group and calls safe
`setsid` before runtime creation, thread creation, current-system reads or
deployment effects. The parent waits once and mirrors its result. Production
always reads the fixed `/run/current-system` link; the private typed fixture
path is compiled only into the harness-free test artifact.
The fixture records successful waits at the common parent/switch wait sites;
these records and its pre/post-detach barriers are absent from installed helpers.
Its external subreaper retains ownership through reporting failures. Native
qualification matches child identities and results to successful waits and
rejects an adopted switch child as an ownership failure. Explicit fixture group
HUP has no controlling TTY and does not establish real SSH disconnect behavior.

The worker has null stdin and no operator transport cancellation token. It owns
each switch child, drains both pipes to completion and reaps it. Broken logging
pipes do not stop the watchdog or close switch output early. The original
nonzero-switch-status quirk remains: after waiting, the helper continues the
confirmation protocol regardless of the switch child's native exit status.
Infrastructure errors retain owned sources and ordered secondary failures.

The check file moves through `switching`, `switched` and external `confirmed`.
Confirmation removes the file and exits 0; timeout invokes the captured old
system with the same action and exits 1. Its observation loop retains the
original half-second cadence and wall-clock timeout ordering. Successful detach
supports survival of ordinary original-session/transport loss. Pre-detach loss,
cgroup-wide killing, targeted worker SIGKILL, reboot and power loss carry no
survival guarantee. Native evidence must establish the bounded contract.

## Kexec and HTTP

Exec/unload avoid HTTP client/runtime creation and retain `--no-sync`. Loading
requires the original `httproot` command-line value even with an explicit server
URL. Default index derivation remains HTTP. Machine, generation, variant and
interactive selection preserve their separate rules and prior download order;
a missing initrd URL is discovered after the kernel download. Missing packaged
interactive default is a failure, with no first-machine fallback.

Short options consume the entire cluster, with argument-taking options owning
the remaining text or the next argument. Unknown switches, attached values on
no-argument long flags, and the exec/unload conflict fail before boot effects.

The kexec executable receives direct ordered arguments: `--load`, the kernel
path, one `--initrd=<path>` argument, one `--append=<unchanged joined params>`
argument, and `--no-sync`. The existing printed command is diagnostic text.
Shell metacharacters in parameters remain literal. Temporary files are checked
and removed after use. Transfer failures retain their primary source and ordered
failed-cleanup paths; ordinary successful-operation cleanup warnings keep their
separate nonfatal policy.

The helper client requires verified TLS for HTTPS images and indexes, rejects
redirects and retains HTTP interoperability. This fixes the old HTTPS-image
plaintext defect without changing the old client's capabilities. HTTPS-only
publication cannot assume that old clients gain the correction.

Connection establishment and response reads each have a 60-second timeout.
The read timeout covers waiting for response headers and resets after each
successful body read; there is no whole-image deadline. Reqwest does not expose
a separate write timeout here, so this does not establish Ruby's write-timeout
parity. Its existing default retries for safe protocol errors remain unchanged;
the timeouts do not promise one network attempt or a total operation deadline.

Automatic reqwest codecs are disabled. Requests preserve Ruby's accepted
encoding header, and the private decoder uses the whole Content-Encoding value,
case-insensitive gzip/x-gzip/deflate matching, Content-Range bypass and actual
gzip/zlib prefix detection. It streams owned chunks without a detached pump or
whole-image buffer. Decoder EOF is followed by checked HTTP framing completion.
Exact empty/trailing/multiple-member behavior remains a qualification boundary;
decoder defaults and a checked transport drain alone do not establish parity.

## Compatibility evidence

[Bounded diagnostic exceptions](helper-diagnostics.md) identify the only approved
ordinary diagnostic changes. Other unobserved parser/I/O groups do not inherit
those exceptions. Original raw captures, classes and byte expectations remain
immutable, including the full 52-entry first-publication failure result.

[Qualification support](../tests/compat/helpers/qualification/README.md) records
the JSON/codec/watchdog native construction and its limits. Wrapper invocations,
real lock contention, interrupted publication, HTTP/TLS/proxy/codecs, transport
loss, checked cleanup, old/new protocol combinations and actual package closures
require their own focused evidence. Neither an uncompiled source export nor an
original-library probe makes the helpers ready for product selection.
