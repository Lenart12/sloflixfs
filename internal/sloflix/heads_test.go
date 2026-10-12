package sloflix

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A new title's probe saves the file's start through its index (moov) plus headSlack; a file with the
// index at the end gets none.
func TestSaveHead(t *testing.T) {
	box := func(typ string, n int) []byte {
		b := make([]byte, n)
		binary.BigEndian.PutUint32(b, uint32(n))
		copy(b[4:], typ)
		for i := 8; i < n; i++ {
			b[i] = byte(i)
		}
		return b
	}
	start := append(box("ftyp", 32), box("moov", 1<<20)...)
	files := map[string][]byte{
		"/start": slices.Concat(start, box("mdat", 20<<20)),
		"/end":   slices.Concat(box("ftyp", 32), box("mdat", 20<<20), box("moov", 1<<20)),
		// A 64-bit size near the maximum, then data: must be rejected, not copied until the timeout.
		"/huge": slices.Concat(box("ftyp", 32), []byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, make([]byte, 20<<20)),
		// Not an MP4 (Matroska's magic): its first "box size" is far over headMax.
		"/mkv": slices.Concat([]byte{0x1a, 0x45, 0xdf, 0xa3}, make([]byte, 20<<20)),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(files[r.URL.Path]))
	}))
	defer srv.Close()
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "heads"), 0755)

	if err := fetchHead(1, srv.URL+"/start", int64(len(files["/start"]))); err != nil {
		t.Fatalf("start: %v", err)
	}
	head, _ := os.ReadFile(headFile(1))
	if want := files["/start"][:len(start)+headSlack]; !bytes.Equal(head, want) {
		t.Fatalf("head is %d bytes, want the first %d", len(head), len(want))
	}
	if err := fetchHead(2, srv.URL+"/end", int64(len(files["/end"]))); err == nil {
		t.Fatal("end: saved")
	}
	if err := fetchHead(2, srv.URL+"/start", 1<<30); err == nil {
		t.Fatal("a file of another size than the crawler saw: saved")
	}
	if _, err := os.Stat(headFile(2)); !os.IsNotExist(err) {
		t.Fatalf("index at the end saved a head: %v", err)
	}
	// Rejected from the box header, not after copying until EOF or the timeout.
	for _, path := range []string{"/huge", "/mkv"} {
		if err := saveHead(3, bytes.NewReader(files[path])); err == nil || !strings.Contains(err.Error(), "not an MP4") {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(cacheDir, "heads")); len(left) != 1 {
		t.Fatalf("heads dir has %d files, want 1 (no temp files left)", len(left))
	}

	// saveHeads saves a head only for an unprobed new title whose link is about to expire; until then the
	// link in meta serves it, also after a restart.
	os.MkdirAll(filepath.Join(cacheDir, "want"), 0755)
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	defer func(n int) { probeCache = n }(probeCache)
	probeCache = 10
	for id, age := range map[int]time.Duration{4: urlTTL - headLead/2, 5: time.Hour, 6: urlTTL + time.Minute} {
		writeMeta(id, Meta{Size: int64(len(files["/start"])), URL: srv.URL + "/start", URLAt: time.Now().Add(-age)})
		writeFile(wantFile(id), nil)
	}
	saveHeads()
	for id, want := range map[int][2]bool{4: {true, false}, 5: {false, true}, 6: {false, false}} {
		_, headErr := os.Stat(headFile(id))
		_, wantErr := os.Stat(wantFile(id))
		if got := [2]bool{headErr == nil, wantErr == nil}; got != want {
			t.Fatalf("title %d: head, marker %v, want %v", id, got, want)
		}
	}
	if u, size, err := StreamURL(5, false); err != nil || u != srv.URL+"/start" || size != int64(len(files["/start"])) {
		t.Fatalf("StreamURL from meta: %q %d %v", u, size, err)
	}
}
