package sloflix

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/cdn"
)

// minVideoSize is the smallest file treated as a video. Smaller ones are broken uploads (seen: 32 KB and
// 64 KB films, a 1.6 MB episode) that ffprobe can't read, so they're hidden like titles without a source.
const minVideoSize = 5 << 20

// fetchHead saves the start of title id's file from stream (see saveHead), if the file is still size bytes.
func fetchHead(id int, stream string, size int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // up to ~70 MB, usually ~16
	defer cancel()
	resp, err := cdn.GetRange(ctx, stream, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got := cdn.RangeSize(resp); got != size { // replaced or gone since the crawl: the probe streams it instead
		return fmt.Errorf("file is %d bytes, was %d (%s)", got, size, resp.Status)
	}
	return saveHead(id, resp.Body)
}

const (
	// headSlack is what Jellyfin's probe reads past the MP4 index: Jellyfin passes ffprobe no -probesize,
	// so its default of 5 MB applies, plus up to 1 MiB of kernel readahead (MaxReadAhead), plus margin.
	headSlack = 7 << 20
	headMax   = 64 << 20 // an index this far in means it isn't a sane MP4 start
	headTTL   = 3 * 24 * time.Hour
	headLead  = time.Hour // how long before urlTTL saveHeads saves a head; above the crawler's rebuild interval
)

// headFile is where a title's probe head is saved (see saveHead).
func headFile(id int) string { return filepath.Join(cacheDir, "heads", fmt.Sprint(id)) }

// wantFile marks a new title Jellyfin hasn't probed yet (see saveHeads).
func wantFile(id int) string { return filepath.Join(cacheDir, "want", fmt.Sprint(id)) }

// OpenHead opens title id's saved start (see saveHead), if it has one, and returns its length.
func OpenHead(id int) (*os.File, int64, bool) {
	f, err := os.Open(headFile(id))
	if err != nil {
		return nil, 0, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, false
	}
	return f, st.Size(), true
}

// Probed records that title id was opened: Jellyfin probes a file once, later opens stream it, so neither
// its saved start nor a later one (saveHeads) is needed.
func Probed(id int) {
	os.Remove(headFile(id))
	os.Remove(wantFile(id))
}

var headBusy sync.Mutex // one saveHeads at a time

// saveHeads saves the start of new titles Jellyfin hasn't probed while their stream link is about to
// expire. Until then a probe uses the link; after, it would need a new lookup (for an embed-only title a
// DoodStream one, which a scan probing many titles runs out of). It also removes heads never probed.
func saveHeads() {
	if !headBusy.TryLock() {
		return
	}
	defer headBusy.Unlock()
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	for _, e := range heads {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > headTTL {
			os.Remove(filepath.Join(cacheDir, "heads", e.Name()))
		}
	}
	want, _ := os.ReadDir(filepath.Join(cacheDir, "want"))
	for _, e := range want {
		id, _ := strconv.Atoi(e.Name())
		m, _, _ := readMeta(id)
		age := time.Since(m.URLAt)
		if m.URL != "" && age < urlTTL-headLead {
			continue
		}
		if m.URL != "" && age < urlTTL && headCount() < probeCache {
			if err := fetchHead(id, m.URL, m.Size); err != nil {
				log.Printf("media %d: no probe cache: %v", id, err) // Jellyfin's probe streams it instead
			}
		}
		os.Remove(wantFile(id))
	}
}

// headCount is the number of saved heads, bounded by -probe-cache so they can't fill the disk before
// Jellyfin's next scan probes (and removes) them.
func headCount() int {
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	return len(heads)
}

// saveHead saves an MP4's start, through its index (the moov box) plus headSlack, to headFile(id), so
// Jellyfin's probe of a new title is served from disk without a stream link (stream.Read). Files with
// the index at the end get none.
func saveHead(id int, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Join(cacheDir, "heads"), "tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there's nothing left to remove
	defer f.Close()
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
		if typ == "mdat" || size < n || size > headMax-off { // not off+size: a huge 64-bit size would overflow
			return fmt.Errorf("not an MP4 with its index (moov) at the start: %q box of %d bytes at %d", typ, size, off)
		}
		f.Write(hdr[:n])
		if _, err := io.CopyN(f, r, size-n); err != nil {
			return err
		}
		if off += size; typ == "moov" {
			break
		}
	}
	if _, err := io.CopyN(f, r, headSlack); err != nil && err != io.EOF {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), headFile(id))
}
