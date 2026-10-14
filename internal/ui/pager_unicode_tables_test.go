package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
)

func TestPinnedRawResourcesAndIndependentGraphemeMapping(t *testing.T) {
	for _, test := range []struct {
		name, hash string
		size       int
	}{{"grapheme_break_property", "260905cffb67055bcaafdf5c32b7e0a347e3be581a57c0c3c9121f65fd3dbd87", 134316}, {"display_width.marshal.gz", "7a49a487a92ae5fd07e236efd3c970c49edca7f87cf168f705f1a6aa31e354a5", 1743}} {
		data, err := os.ReadFile("testdata/pager-source/" + test.name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if len(data) != test.size || hex.EncodeToString(sum[:]) != test.hash {
			t.Fatalf("fixed source changed: %s", test.name)
		}
		if test.name == "grapheme_break_property" {
			// Parse the original seven-byte loader records directly, applying
			// last-value-wins before comparing every stored point and gap.
			mapping := map[rune]int{}
			for offset := 0; offset < len(data); offset += 7 {
				cp, err := strconv.ParseInt(string(data[offset:offset+6]), 16, 32)
				if err != nil {
					t.Fatal(err)
				}
				property, err := strconv.ParseInt(string(data[offset+6:offset+7]), 16, 8)
				if err != nil {
					t.Fatal(err)
				}
				mapping[rune(cp)] = int(property)
			}
			for codepoint := rune(0); codepoint <= 0x10ffff; codepoint++ {
				want, present := mapping[codepoint]
				if !present {
					want = 255
				}
				if got := pagerRangeValue(pagerGraphemeRanges[:], codepoint, 255); got != want {
					t.Fatalf("propertyU+%X=%d want%d", codepoint, got, want)
				}
			}
		}
	}
}

func TestTableStructureAndSourceWidthBoundaries(t *testing.T) {
	for _, rows := range [][]pagerRange{pagerGraphemeRanges[:], pagerWidthRanges[:]} {
		for index, row := range rows {
			if row.first > row.last || index > 0 && rows[index-1].last >= row.first {
				t.Fatalf("overlap/order at %d", index)
			}
		}
	}
	if len(pagerGraphemeRanges) != 1170 || len(pagerWidthRanges) != 478 {
		t.Fatal("fixed coalesced range counts")
	}
	// Literal source-default points include the >15/<161 shortcut, the
	// FIRST_4096 endpoint and both absent trailing planes of the width tree.
	for _, test := range []struct {
		cp   rune
		want int
	}{{0, 0}, {8, -1}, {9, 1}, {15, 0}, {16, 1}, {160, 1}, {161, 1}, {0x300, 0}, {0x1000, 1}, {0x2e3b, 3}, {0x4e00, 2}, {0xf0000, 1}, {0x100000, 1}, {0x10ffff, 1}} {
		if got := pagerRangeValue(pagerWidthRanges[:], test.cp, 1); got != test.want {
			t.Fatalf("widthU+%X=%d want%d", test.cp, got, test.want)
		}
	}
}
