package sloflix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/logx"
	"github.com/Lenart12/sloflixfs/internal/mp4"
)

// fetchHead saves the head of title id's file from stream, if the file is still size bytes.
func fetchHead(id int, stream string, size int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // up to ~70 MB, usually ~16
	defer cancel()
	resp, err := cdn.GetRange(ctx, stream, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if got := cdn.RangeSize(resp); got != size {
		return fmt.Errorf("file is %d bytes, was %d (%s)", got, size, resp.Status)
	}
	return saveHead(id, resp.Body)
}

const (
	// headSlack is what Jellyfin's probe reads past the MP4 index: ffprobe's default 5 MB probe size, 1 MiB
	// of kernel readahead and a margin.
	headSlack = 7 << 20
	headMax   = 64 << 20 // an index this far in means it isn't a sane MP4 start
	headTTL   = 3 * 24 * time.Hour
	headLead  = time.Hour // how long before urlTTL a head is saved; longer than a crawler round
)

// headFile is where a title's head is saved.
func headFile(id int) string { return filepath.Join(cacheDir, "heads", fmt.Sprint(id)) }

// wantFile marks a new title Jellyfin hasn't probed yet.
func wantFile(id int) string { return filepath.Join(cacheDir, "want", fmt.Sprint(id)) }

// OpenHead opens title id's saved head, if it has one, and returns its length.
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

// Probed records that title id was opened. Jellyfin probes a file only once, so its head is no longer
// needed.
func Probed(id int) {
	os.Remove(headFile(id))
	os.Remove(wantFile(id))
}

var headBusy sync.Mutex // one saveHeads at a time

// saveHeads saves the heads of new titles Jellyfin hasn't probed yet whose stream link is about to expire,
// so the probe needs no new lookup, and removes heads never probed.
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
				logx.Warnf("media %d: no probe cache: %v", id, err)
			}
		}
		os.Remove(wantFile(id))
	}
}

// headCount is the number of saved heads, which -probe-cache bounds so they can't fill the disk.
func headCount() int {
	heads, _ := os.ReadDir(filepath.Join(cacheDir, "heads"))
	return len(heads)
}

// saveHead saves an MP4's head, its start through the index plus headSlack, so Jellyfin's probe of a new
// title is served from disk.
func saveHead(id int, r io.Reader) error {
	f, err := os.CreateTemp(filepath.Join(cacheDir, "heads"), "tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op after the rename
	defer f.Close()
	if err := mp4.CopyHead(f, r, headMax); err != nil {
		return err
	}
	if _, err := io.CopyN(f, r, headSlack); err != nil && err != io.EOF {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), headFile(id))
}
