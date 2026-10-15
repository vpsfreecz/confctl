---
title: "confctl-docgen-fixture"
section: "8"
date: "2026-10-14"
footer: "fixture footer"
header: "fixture header"
---

# NAME

confctl-docgen-fixture - finite backend content fixture

# FEATURES

`fixture.option`

:   First definition paragraph sentinel.

    Second definition paragraph sentinel with <angle-sentinel> prose.

    - Nested item sentinel.
    - Second item sentinel.

    *Type:* attribute set

    *Default:*

    ````nix
    {
      "if" = [ (-1) 1.0e3 9007199254740993123456789 ];
      "unsafe key" = "\${literal}";
      "quote\"key" = "backslash\\value";
      nested = [ { value = null; } [] {} false "" ];
    }
    ````

    *Example:*

    ````text
    first literal line

    third literal line with ``` and `edge`
    ````

    *Declared by:* `<confctl/nix/modules/fixture.nix>`

`fixture.empty`

:   *Type:* list

    *Default:*

    ```nix
    []
    ```

## Subsection

| Column | Content |
| --- | --- |
| table-key-sentinel | table-value-sentinel |

[local link sentinel](#subsection),
<https://example.invalid/docgen>, ~~strike-sentinel~~ and super^script-sentinel^.
Unicode sentinel: příliš 東京.

```text
.dot-control-sentinel
'apostrophe-control-sentinel
\backslash-control-sentinel
```
