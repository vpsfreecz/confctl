---
title: "confctl"
section: "8"
date: "2026-10-09"
footer: "fixture-version"
header: "confctl"
---

# NAME

`confctl` - Nix deployment configuration management tool

# SYNOPSIS

`confctl` [global options] command [command options] [arguments...]

# DESCRIPTION

`confctl` manages configurations for NixOS and vpsAdminOS machines. Its command
declarations cover configuration, input management, build and deployment work.
The reference below includes commands unavailable in the experimental Go tool.
Each command and group states its availability in the help below. The command
list alone does not show that every handler works.

# FLAKE INPUTS

Configurations provide `flake.nix` outputs through `confctl.lib.mkConfctlOutputs`
and keep dependency revisions in `flake.lock`. Channels map dependency roles to
root inputs. Machines select channels through `cluster.<name>.inputs.channels`;
later channels override earlier mappings, and `inputs.overrides` takes
precedence over channels. Build and deploy do not update locked inputs
automatically.

Software-pin configurations require migration with retained confctl v3 before
upgrading to the flake-only tool. The
[flake input guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/docs/flake-inputs.md)
and [migration and rollback guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/docs/swpins-to-flakes.md)
describe the configuration and retained-closure requirements.

# COMMANDS

## `confctl`

```text
NAME
    confctl - Manage fixture systems


SYNOPSIS
    confctl [global options] command [command options]


GLOBAL OPTIONS


COMMANDS
    inspect - Inspect declared fixtures
```

## `confctl inspect`

```text
NAME
    inspect - Inspect declared fixtures

SYNOPSIS

    confctl [global options] inspect [command options] [name]

COMMAND OPTIONS
    -n, --name=arg - Selected name (default: value)

```

# GENERATION SELECTION AND ROLLBACK

Generation offsets refer to the selected ordered list: `0` selects its last
record, `1` its first, `2` its second and `-1` the record before last. Offset `0`
does not resolve the local `current` link. Explicit `current` selection uses the
resolved current record; an absent local link permits the reader's in-memory
newest-supported choice, while an unresolved link fails current selection.

Local lists and retention count supported flake records. Unsupported or invalid
records are reported and left untouched, including their roots. Numeric local
selection fails when records were excluded; select an explicit supported name.
Local old-generation removal and rotation require resolved current state.
Remote-only profile selection does not depend on a local current link.
Execution availability remains as declared in each command's help above.

Retain the previous configuration revision, lock, executable environment and
required local and remote roots for rollback. Selecting an older tool does not
reconstruct removed records, profile entries or collected store closures. The
[migration and rollback guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/docs/swpins-to-flakes.md)
describes the supported format and recovery limits.

# EXECUTABLE EXTENSIONS

The [executable extension SDK](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/extension/README.md)
and [composition guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/experiments/confctl-go/README.md)
remain experimental. The guides describe explicit registration. Generating this
reference does not load Ruby user scripts or discover operational extensions.

The [configuration extension guide](https://github.com/vpsfreecz/vpsfree-cz-configuration/blob/0c557149fd4e6e653cd876b16382559efbb548e4/extensions/README.md)
describes netboot rediscovery and runtime-kernel workflows. These site workflows
are separate from the builtin commands.
