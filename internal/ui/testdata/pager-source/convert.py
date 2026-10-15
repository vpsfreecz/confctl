"""One-time conversion of the two fixed pager resources; never a build step."""

from bisect import bisect_right
import gzip
import hashlib
import json
from pathlib import Path

HERE = Path(__file__).parent
MAX_CP = 0x10FFFF


def fixed(name, size, digest):
    data = (HERE / name).read_bytes()
    assert len(data) == size and hashlib.sha256(data).hexdigest() == digest, name
    return data


class FixedWidthData:
    # Only the inert tokens in the named resource are admitted. No Ruby code,
    # class, instance variable, user object, string, or dynamic loader is used.
    def __init__(self, data):
        assert data[:2] == b"\x04\x08"
        self.data, self.pos = data, 2
        self.objects, self.symbols = [], []

    def byte(self):
        assert self.pos < len(self.data), "truncated token"
        value = self.data[self.pos]
        self.pos += 1
        return value

    def integer(self):
        n = self.byte()
        if n >= 128:
            n -= 256
        if n == 0:
            return 0
        if n > 4:
            return n - 5
        if n < -4:
            return n + 5
        count = abs(n)
        value = 0
        for index in range(count):
            value |= self.byte() << (8 * index)
        return value if n > 0 else value - (1 << (8 * count))

    def value(self):
        tag = self.byte()
        if tag == ord("0"):
            return None
        if tag == ord("i"):
            return self.integer()
        if tag == ord("["):
            count = self.integer()
            assert 0 <= count <= 17, count
            result = []
            self.objects.append(result)
            for _ in range(count):
                result.append(self.value())
            return result
        if tag == ord(":"):
            count = self.integer()
            assert count == 1 and self.byte() == ord("A")
            self.symbols.append("A")
            return "A"
        if tag == ord(";"):
            index = self.integer()
            assert 0 <= index < len(self.symbols)
            return self.symbols[index]
        if tag == ord("@"):
            index = self.integer()
            assert 0 <= index < len(self.objects)
            return self.objects[index]
        raise ValueError(f"unsupported fixed-table token {tag:#x}")


def padded(values):
    # index.rb's value[15] ||= nil modifies the shared loaded arrays.
    while len(values) < 16:
        values.append(None)
    return values


def decompress(values, level):
    result = []
    for value in values:
        if level > 0:
            result.extend(decompress(padded(value) if isinstance(value, list)
                                     else [value] * 16, level - 1))
        else:
            result.extend(padded(value) if isinstance(value, list) else [value] * 16)
    return result


def effective(value):
    return 1 if value is None or value == "A" else value


def ranges(values, default):
    result = []
    for cp, value in sorted(values.items()):
        if value == default:
            continue
        if result and result[-1][1] + 1 == cp and result[-1][2] == value:
            result[-1][1] = cp
        else:
            result.append([cp, cp, value])
    return result


def lookup(rows, cp, default):
    starts = [row[0] for row in rows]
    index = bisect_right(starts, cp) - 1
    return rows[index][2] if index >= 0 and cp <= rows[index][1] else default


def main():
    raw = fixed("grapheme_break_property", 134316,
                "260905cffb67055bcaafdf5c32b7e0a347e3be581a57c0c3c9121f65fd3dbd87")
    assert len(raw) % 7 == 0
    properties = {}
    for offset in range(0, len(raw), 7):
        # read_hexdigit_map is last-value-wins, not an assumed sorted list.
        cp, prop = int(raw[offset:offset + 6], 16), int(raw[offset + 6:offset + 7], 16)
        assert cp <= MAX_CP and 0 <= prop <= 11
        properties[cp] = prop
    grapheme = ranges(properties, 255)
    for cp, prop in properties.items():
        assert lookup(grapheme, cp, 255) == prop
    for left, right, _ in grapheme:
        for cp in (left - 1, right + 1):
            if 0 <= cp <= MAX_CP:
                assert lookup(grapheme, cp, 255) == properties.get(cp, 255)

    zipped = fixed("display_width.marshal.gz", 1743,
                   "7a49a487a92ae5fd07e236efd3c970c49edca7f87cf168f705f1a6aa31e354a5")
    data = gzip.decompress(zipped)
    assert len(data) == 10988 and hashlib.sha256(data).hexdigest() == \
        "ec39283a27a3f589d34a908ee1dfbbca50f9fd9a9b94d962a992c20de308ca4d"
    parser = FixedWidthData(data)
    index = parser.value()
    assert parser.pos == len(data), "trailing data"
    assert isinstance(index, list) and len(index) == 15
    first = decompress(index[0][0], 1)

    # This follows the original iterative lookup independently of range
    # expansion. The <0x1001 test deliberately includes the padding boundary.
    def source_width(cp):
        if 15 < cp < 161:
            return 1
        if cp < 0x1001:
            return effective(first[cp] if cp < len(first) else None)
        value, depth = index, 0x10000
        while isinstance(value, list):
            slot = cp // depth
            value = value[slot] if slot < len(value) else None
            if isinstance(value, list):
                cp %= depth
                depth //= 16
                assert depth > 0
        return effective(value)

    # Independent recursive expansion of the actual stored tree. Missing
    # children map to nil; source defaults/shortcut/padding are applied after.
    values = [1] * (MAX_CP + 1)

    def expand(value, start, span):
        if isinstance(value, list):
            assert span >= 16 and len(value) <= 16
            for slot in range(16):
                expand(value[slot] if slot < len(value) else None,
                       start + slot * (span // 16), span // 16)
        else:
            assert value is None or value == "A" or isinstance(value, int)
            values[start:start + span] = [effective(value)] * span

    for slot, value in enumerate(index):
        expand(value, slot * 0x10000, 0x10000)
    for cp in range(0x1001):
        values[cp] = effective(first[cp] if cp < len(first) else None)
    values[16:161] = [1] * 145
    width = ranges(dict(enumerate(values)), 1)
    starts = [row[0] for row in width]
    for cp in range(MAX_CP + 1):
        position = bisect_right(starts, cp) - 1
        generated = width[position][2] if position >= 0 and cp <= width[position][1] else 1
        assert generated == source_width(cp), hex(cp)
    for rows in (grapheme, width):
        assert all(left <= right for left, right, _ in rows)
        assert all(rows[i - 1][1] < rows[i][0] for i in range(1, len(rows)))

    header = """// Code generated once from the fixed pager resources by testdata/pager-source/convert.py; DO NOT EDIT.
// Grapheme properties: unicode_utils 1.4.0 / Unicode 6.2.0.
// Width defaults: unicode-display_width 2.6.0 / Unicode 16.0.0.
// See pager_source.NOTICE. Compiler Unicode tables do not select these values.
package ui

type pagerRange struct { first, last rune; value int }

"""
    chunks = [header]
    for name, rows in (("pagerGraphemeRanges", grapheme), ("pagerWidthRanges", width)):
        chunks.append(f"var {name} = [...]pagerRange{{\n")
        chunks.extend(f"\t{{0x{left:x}, 0x{right:x}, {value}}},\n" for left, right, value in rows)
        chunks.append("}\n\n")
    chunks.append("""func pagerRangeValue(rows []pagerRange, codepoint rune, fallback int) int {
    low, high := 0, len(rows)
    for low < high {
        middle := low + (high-low)/2
        if rows[middle].last < codepoint { low = middle+1 } else { high = middle }
    }
    if low < len(rows) && rows[low].first <= codepoint { return rows[low].value }
    return fallback
}
""")
    output = "".join(chunks).encode()
    destination = HERE.parent.parent / "pager_unicode_tables.go"
    with destination.open("xb") as stream:
        stream.write(output)
    print(json.dumps({"source_records": len(raw) // 7,
                      "effective_grapheme_entries": len(properties),
                      "grapheme_ranges": len(grapheme), "width_ranges": len(width),
                      "first_decompressed_entries": len(first),
                      "width_effective_values": sorted(set(values)),
                      "source_root_entries": len(index),
                      "trailing_default_planes": 17 - len(index),
                      "finite_width_points_verified": MAX_CP + 1,
                      "whole_fixed_loader_consumption": True,
                      "independent_recursive_vs_original_iterative_lookup": True,
                      "output_bytes_before_gofmt": len(output),
                      "output_sha256_before_gofmt": hashlib.sha256(output).hexdigest()}, indent=2))


if __name__ == "__main__":
    main()
