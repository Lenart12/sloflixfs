// Package mp4 reads the start of MP4 files.
package mp4

import (
	"encoding/binary"
	"fmt"
	"io"
)

// CopyHead copies the boxes from r to w up to and including the index (moov). It fails if the index isn't
// within the first max bytes, which includes files with the index after the media data (mdat).
func CopyHead(w io.Writer, r io.Reader, max int64) error {
	for off := int64(0); ; {
		var hdr [16]byte
		if _, err := io.ReadFull(r, hdr[:8]); err != nil {
			return err
		}
		size, n := int64(binary.BigEndian.Uint32(hdr[:4])), int64(8)
		if size == 1 { // 64-bit size
			if _, err := io.ReadFull(r, hdr[8:]); err != nil {
				return err
			}
			size, n = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		}
		typ := string(hdr[4:8])
		if typ == "mdat" || size < n || size > max-off { // not off+size: a huge 64-bit size would overflow
			return fmt.Errorf("not an MP4 with its index (moov) at the start: %q box of %d bytes at %d", typ, size, off)
		}
		if _, err := w.Write(hdr[:n]); err != nil {
			return err
		}
		if _, err := io.CopyN(w, r, size-n); err != nil {
			return err
		}
		if off += size; typ == "moov" {
			return nil
		}
	}
}
