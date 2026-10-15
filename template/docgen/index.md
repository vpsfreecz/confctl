# Manuals

| Reference | Contents |
| --- | --- |
| [confctl(8)](confctl.8.html) | Declared commands, options and execution availability |
| [confctl-options.nix(8)](confctl-options.nix.8.html) | Public cluster and machine system options |
| [kexec-netboot(8)](kexec-netboot.8.html) | Netboot helper selection, transport and file behavior |

These pages share the adjacent [stylesheet](style.css). The command reference
includes unavailable and conditional commands; a listed command may not have a
working handler.

# Extension and configuration guides

The [experimental executable SDK](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/extension/README.md)
and [experimental CLI composition guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/experiments/confctl-go/README.md)
describe the executable extension boundary and opt-in composition.

The [vpsFree.cz configuration extension guide](https://github.com/vpsfreecz/vpsfree-cz-configuration/blob/0c557149fd4e6e653cd876b16382559efbb548e4/extensions/README.md)
describes netboot rediscovery and runtime-kernel update/preparation. These
workflows belong to the configuration and are separate from the 29 builtin
commands. Generating these pages does not discover or run a configuration
registry.
