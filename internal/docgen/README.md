# Markdown generation

`cmd/confctl-docs` generates command and Nix option reference Markdown. It takes
an explicit version and date, so repeating a run with the same declarations and
metadata produces the same document.

The output uses the selected Pandoc Markdown dialect. Its YAML metadata contains
the title, section `8`, date, footer (version) and header `confctl`, with strings
encoded as JSON string literals. Manual sections use level-one headings and
command sections use level-two headings.

```sh
confctl-docs --kind commands --version VERSION --date YYYY-MM-DD \
  --output man/man8/confctl.8.md
confctl-docs --kind options --version VERSION --date YYYY-MM-DD \
  --source-root CONFCTL_SOURCE --output man/man8/confctl-options.nix.8.md
```

The options command evaluates `CONFCTL_SOURCE#moduleOptions`. Supply
`--module-options FILE` to read a decoded JSON input instead. Command generation
uses the existing typed registry and its help renderer, including availability,
options and defaults. It does not evaluate Nix or execute extensions.

The options renderer selects public `confctl.` and `cluster.` declarations and
sorts each section by option name. It preserves the confctl, machine metadata
and machine system sections from the options template. Descriptions retain the
source's `<literal>`/`<option>` conversions and HTML entity decoding. Confctl
module declarations under the selected source root or a Nix store source become
`<confctl/nix/modules/...>`; other declaration paths retain their source text.

Each option is a definition-list term with its description, Type, Default,
present Example and Declared by blocks in one definition. Description paragraphs
and relative indentation are preserved. Empty descriptions still get a
definition; duplicate option names remain separate entries in source order.
Default and present Example values always use fenced `nix` blocks. The fence has
at least three backticks and is longer than any backtick run in the value. The
block preserves the complete value and its relative indentation; only a missing
line separator before the closing fence is added. Multiline literal and option
tags use fenced `text` blocks and split surrounding prose into paragraphs.
Single-line tags keep safe inline delimiters. Literal tags are converted before option tags, then
entities are decoded once.

Nix values use the injected shared `core.DocumentationNixLiteral` formatter;
the [extension contract](../../extension/README.md) owns its scalar encoding.
Maps sort keys and use bare keys only when every key in that map meets the
original ASCII rule and is not a reserved word. A reserved key such as `if`
quotes every key in its map; child maps choose independently. The attribute
names `or`, `true`, `false` and `null` remain eligible for bare keys. The SDK/RPC
formatter uses the same scalar and list encoding and keeps its quote-all-keys
policy.

Decoded number tokens retain their digits, signs and exponent spelling without
conversion to `float64`. When an exponent token has no decimal point in its
mantissa, the formatter inserts `.0`: `1e3` becomes `1.0e3` so Nix parses it as a
float. Finite Go `float64` values retain floating syntax, including `1.0` and
negative zero `-0.0`. Negative numeric list entries are grouped: `[ -1 2 ]`
becomes `[ (-1) 2 ]`; the string `"-1"` stays a string. Preserving source digits
does not establish that a number fits the target Nix integer range or that its
floating arithmetic is exact. The formatter does not convert an out-of-range
integer to a float.

`literalExpression` values contain trusted Nix source. Their text passes through
unchanged and bypasses data encoding and validation, including the number and
key rules above.
Examples omit null and false values and include zero, an empty string and an
empty list.

Literal formatting follows these rules:

- An embedded backtick needs a longer Markdown delimiter. The
  single-delimiter text `` `notes`log` `` becomes ```` ``notes`log`` ````.
  Literals with backticks at their edges also need padding: ```` `` `edge` `` ````.
  Multiline display literals use fenced blocks to avoid inline-code newline
  folding by the backend.
- Nix strings and quoted keys escape interpolation, quotes and backslashes.
  For example, the source value `${name}` becomes `"\${name}"` rather than
  `"${name}"`. A key containing a quote is escaped inside the quoted key.
  Already-Nix `literalExpression` text is intentionally unchanged.

Encoded strings and keys must be valid UTF-8 and contain no NUL. LF, CR and TAB
use Nix's `\n`, `\r` and `\t` escapes. Other representable controls, including
backspace, form feed, ESC and DEL, retain literal bytes; Go-only `\b`, `\f`,
`\xNN` and `\uNNNN` escapes would change their Nix meaning. Valid non-ASCII
UTF-8 stays unchanged. NUL and invalid UTF-8 in directly supplied Go strings
return errors, including in nested values and quoted keys. Malformed direct
`json.Number` values also return errors. The existing JSON decoder retains its
own decoding behavior.

File output follows the fixed `.new` sibling contract. Generation finishes
before the sibling opens. A formatter error names the option, emits no partial
stdout and leaves the destination and existing sibling untouched, including
aliases. Writing and closing finish before rename replaces the destination.
Errors retain the file effects and sibling residue produced by the failed
operation. If the sibling does not alias the destination, the destination remains
unchanged until rename succeeds. Existing sibling symlinks follow ordinary write
semantics. The writer creates no parent directories, removes no residue and adds
no retry or rollback. Reusing an existing sibling retains its file permissions
under ordinary write rules.

The original `testdata/commands.md`, `options.golden.json` and
`module-options.json` remain unchanged historical source fixtures. New layout
expectations live in `testdata/pandoc`. They derive from the same registry help,
`lib/confctl/module_options.rb`, options ERB templates and Rake behavior, with the
Pandoc presentation described above. They are synthetic inputs and authored
expectations, not observations of the Ruby tool or Pandoc. Focused tests check
all 29 product declarations and separate framework scopes, each scope's complete
Help output, selected module terms, declaration changes, ordered renderer calls,
literal values, escaping and each file failure phase without running Nix.

The generator writes Markdown. Roff and HTML rendering and documentation
package composition belong to separate consumers, which need their own content,
structure and deterministic-rendering checks. The kexec manual has its focused
backend producer. Synthetic Markdown goldens do not establish rendered structure
or installed package contents.

## Documentation tool and bundle

`nix/docgen-tool.nix` takes `{ pkgs, src ? ../. }` and builds only
`bin/confctl-docs`. Its clean source includes the imported core, CLI, docgen and
extension packages, embedded context and focused fixtures. It uses the supplied
`pkgs.buildGoModule`, the existing Go module and `vendorHash = null`; it does not
choose another compiler or change CGO. Its check phase runs the focused Go tests
and vet with optional external tiers unset.

`nix/docs.nix` takes `{ pkgs, src ? ../., moduleOptions, version, date }`.
The caller supplies `moduleOptions` as an evaluated declaration list, which the
derivation writes to a JSON input file. One generator derivation writes commands
and options with explicit version/date through the existing file-input path. It
performs no nested Nix evaluation or extension discovery. The command document
embeds fixed Name, Synopsis, Description, flake-input and generation/rollback context; option
spellings and command availability continue to come from Registry.Help.

The bundle imports the shared renderer in `nix/docgen.nix` for separate man and
HTML derivations and reuses `nix/kexec-netboot-man.nix` for the helper man file.
The helper HTML uses that same Markdown and its fixed date `2026-10-14`, footer
and header `kexec-netboot`. The final derivation only copies regular files:

| Directory beneath the bundle | Files |
| --- | --- |
| `share/man/man8` | `confctl.8`, `confctl-options.nix.8`, `kexec-netboot.8` |
| `share/doc/confctl/html` | `confctl.8.html`, `confctl-options.nix.8.html`, `kexec-netboot.8.html`, `index.html`, `style.css` |
| `share/doc/confctl/markdown` | `confctl.8.md`, `confctl-options.nix.8.md`, `kexec-netboot.8.md` |

The bundle contains uncompressed manuals. Use its `share/man` as a MANPATH entry
and its HTML index to navigate the three pages. Manual links and the stylesheet
are local; links to SDK/composition and configuration extension guides point to
pinned source revisions. Netboot and runtime-kernel workflows belong to the
configuration and are separate from the 29 builtin commands. The bundle contains
no symlinks to a source tree or renderer executable.

The flake exposes `confctl-docgen`, `confctl-docs` and `kexec-netboot-man`.
The normal `confctl` package includes the documentation bundle, and normal
shells add its manuals to MANPATH. Build the separate bundle with
`nix build .#confctl-docs`; open `result/share/doc/confctl/html/index.html` for
the HTML reference. Ordinary checks include the focused renderer fixture.
The retained Ruby generator belongs to the explicit original-tool lane.

Consumers must check rendered structure/content, independent repeat renders,
actual module-options coverage, installed links/modes, requisite closures and
both supported architectures for the revision they build. Markdown tests alone
do not verify these properties. Literal values and declaration provenance can
retain store references; preserve them and report closure findings instead of
rewriting the source to hide them.
