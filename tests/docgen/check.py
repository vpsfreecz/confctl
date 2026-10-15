"""Finite post-review checks of real Pandoc/groff outputs; no Markdown parser."""

import json
import re
import sys
from html.parser import HTMLParser
from pathlib import Path


class Document(HTMLParser):
    def __init__(self, source):
        super().__init__(convert_charrefs=True)
        self.root = {"tag": "root", "attrs": {}, "children": []}
        self.stack = [self.root]
        self.feed(source)
        self.close()

    def handle_starttag(self, tag, attrs):
        node = {"tag": tag, "attrs": dict(attrs), "children": []}
        self.stack[-1]["children"].append(node)
        if tag not in {"meta", "link", "br", "hr", "img", "input", "source"}:
            self.stack.append(node)

    def handle_endtag(self, tag):
        for i in range(len(self.stack) - 1, 0, -1):
            if self.stack[i]["tag"] == tag:
                del self.stack[i:]
                return

    def handle_data(self, data):
        self.stack[-1]["children"].append(data)


def elements(node):
    if isinstance(node, dict):
        yield node
        for child in node["children"]:
            yield from elements(child)


def text(node):
    if isinstance(node, str):
        return node
    return "".join(text(child) for child in node["children"])


def pandoc_nodes(value):
    if isinstance(value, dict):
        if "t" in value:
            yield value
        for child in value.values():
            yield from pandoc_nodes(child)
    elif isinstance(value, list):
        for child in value:
            yield from pandoc_nodes(child)


def main(paths):
    assert len(paths) == 6
    ast, roff, html, visible, manual, manual_visible = [
        Path(path).read_text(encoding="utf-8") for path in paths
    ]
    nodes = list(pandoc_nodes(json.loads(ast)))
    definitions = [node for node in nodes if node["t"] == "DefinitionList"]
    assert sum(len(node["c"]) for node in definitions) == 2
    assert any(node["t"] == "Table" for node in nodes)
    assert all(any(node["t"] == kind for node in nodes) for kind in ["Strikeout", "Superscript"])
    code = [node["c"][1] for node in nodes if node["t"] == "CodeBlock"]
    expected_nix = (
        '{\n  "if" = [ (-1) 1.0e3 9007199254740993123456789 ];\n'
        '  "unsafe key" = "\\${literal}";\n'
        '  "quote\\"key" = "backslash\\\\value";\n'
        '  nested = [ { value = null; } [] {} false "" ];\n}'
    )
    expected_lines = "first literal line\n\nthird literal line with ``` and `edge`"
    expected_control = (
        ".dot-control-sentinel\n'apostrophe-control-sentinel\n"
        "\\backslash-control-sentinel"
    )
    for value in [expected_nix, expected_lines, expected_control, "[]"]:
        assert value in code, ("parsed code lost content", value, code)
    assert re.search(
        r'^\.TH "confctl\\-docgen\\-fixture" "8" "2026\\-10\\-14" '
        r'"fixture footer" "fixture header"$', roff, re.MULTILINE
    ), roff
    assert '.SH NAME' in roff and '.SH FEATURES' in roff
    assert '.SS Subsection' in roff
    assert not any(
        line.startswith((".dot-control-sentinel", "'apostrophe-control-sentinel"))
        for line in roff.splitlines()
    ), "literal control line became a roff request"
    document = Document(html)
    tags = list(elements(document.root))
    assert any(n["tag"] == "title" and text(n) == "confctl-docgen-fixture" for n in tags)
    assert any(n["tag"] == "link" and n["attrs"].get("href") == "style.css" for n in tags)
    assert all(any(n["tag"] == tag for n in tags) for tag in ["html", "head", "body", "h2", "h3", "table", "del", "sup"])
    tables = [n for n in tags if n["tag"] == "table"]
    assert len(tables) == 1, tables
    rows = [n for n in elements(tables[0]) if n["tag"] == "tr"]
    cells = [
        [n for n in row["children"] if isinstance(n, dict) and n["tag"] in {"th", "td"}]
        for row in rows
    ]
    assert [[cell["tag"] for cell in row] for row in cells] == [
        ["th", "th"], ["td", "td"],
    ], cells
    assert [[text(cell) for cell in row] for row in cells] == [
        ["Column", "Content"],
        ["table-key-sentinel", "table-value-sentinel"],
    ], cells
    terms = [text(n) for n in tags if n["tag"] == "dt"]
    assert terms == ["fixture.option", "fixture.empty"], terms
    bodies = [text(n) for n in tags if n["tag"] == "dd"]
    assert any(
        "First definition paragraph sentinel." in body
        and "Second definition paragraph sentinel with <angle-sentinel> prose." in body
        and "Nested item sentinel." in body
        for body in bodies
    ), bodies
    html_code = [text(n) for n in tags if n["tag"] == "code"]
    for value in [expected_nix, expected_lines, expected_control]:
        assert value in html_code or value + "\n" in html_code, value
    assert any(n["tag"] == "a" and n["attrs"].get("href") == "#subsection" for n in tags)
    assert any(n["tag"] == "a" and n["attrs"].get("href") == "https://example.invalid/docgen" for n in tags)
    for sentinel in [
        "fixture.option", "fixture.empty", "First definition paragraph sentinel.",
        "Second definition paragraph sentinel", "Nested item sentinel.",
        "table-key-sentinel", "table-value-sentinel", "1.0e3",
        "9007199254740993123456789", "first literal line", "third literal line",
        ".dot-control-sentinel", "'apostrophe-control-sentinel",
        "\\backslash-control-sentinel", "příliš", "東京",
    ]:
        assert sentinel in visible, ("groff lost sentinel", sentinel)
    assert visible.index("first literal line") < visible.index("third literal line")
    assert re.search(
        r'^\.TH "kexec\\-netboot" "8" "2026\\-10\\-14" '
        r'"kexec\\-netboot" "kexec\\-netboot"$',
        manual, re.MULTILINE,
    ), manual
    for option in [
        "--help", "--server-url", "--machine", "--generation", "--variant",
        "--interactive", "--append", "--unload", "--exec", "--no-sync",
    ]:
        assert option in manual_visible, ("manual option absent", option)
    print("Pandoc structure, roff/HTML content and kexec manual checks passed")


if __name__ == "__main__":
    main(sys.argv[1:])
