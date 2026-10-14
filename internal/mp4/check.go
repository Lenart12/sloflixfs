package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Check reports whether head, a file's first bytes, can start a video: MP4 boxes, Matroska or MPEG-TS. It
// catches broken uploads that ffprobe can't read: data that isn't a container at all, and an index (moov)
// whose first box claims to be larger than the index itself.
func Check(head []byte) error {
	if bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) || len(head) > 0 && head[0] == 0x47 {
		return nil // Matroska, MPEG-TS
	}
	for off := 0; off+8 <= len(head); {
		size, typ := int(binary.BigEndian.Uint32(head[off:])), head[off+4:off+8]
		if !boxType(typ) {
			return fmt.Errorf("no video container at byte %d: % x", off, head[off:off+8])
		}
		if size < 8 { // 0: to the end of the file, 1: a 64-bit size
			return nil
		}
		if string(typ) == "moov" && size >= 16 && off+16 <= len(head) {
			if child := int(binary.BigEndian.Uint32(head[off+8:])); child > size-8 {
				return fmt.Errorf("index (moov) of %d bytes starts with a %q box of %d bytes", size, head[off+12:off+16], child)
			}
		}
		off += size
	}
	return nil
}

// boxType reports whether b is a box type: four printable ASCII characters.
func boxType(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}
