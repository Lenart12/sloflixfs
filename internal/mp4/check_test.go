package mp4

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		name, hex string
		ok        bool
	}{
		// First bytes of files on orange that ffprobe can't read.
		{"moov with an oversized mvhd (Wild Kratts S01E04)", "0000002066747970 69736f6d00000200 69736f6d69736f32 617663316d703431 0010ca766d6f6f76 0397c1fb6d766864 0000000000000000 00000000000003e8", false},
		{"no container (Dexter S04E03)", "14d34a2888ab43e0 444d2205b79dc86b 6017718026ea04b9 5b80d205b2e48f22", false},
		{"moov first", "0000002066747970 69736f6d00000200 69736f6d69736f32 617663316d703431 0010ca766d6f6f76 0000006c6d766864 0000000000000000 00000000000003e8", true},
		{"mdat first", "0000002066747970 69736f6d00000200 69736f6d69736f32 617663316d703431 0397c1fb6d646174 0000000000000000", true},
		{"QuickTime without ftyp", "0000000877696465 0397c1fb6d646174", true},
		{"Matroska", "1a45dfa3a3428681 0101", true},
		{"MPEG-TS", "474011100042f025", true},
		{"short", "0000", true},
	} {
		b, err := hex.DecodeString(strings.ReplaceAll(c.hex, " ", ""))
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(b); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
