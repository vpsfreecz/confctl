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
