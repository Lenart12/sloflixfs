package sloflix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// The crawler checks new titles first (newest first), then rechecks oldest first, skipping recent failures.
func TestCrawlQueue(t *testing.T) {
	cacheDir = t.TempDir()
	for _, d := range []string{"json", "meta", "heads"} {
		os.MkdirAll(filepath.Join(cacheDir, d), 0755)
	}
	refreshTTL, metaTTL = time.Hour, 24*time.Hour
	mem, crawlFailed = map[string]memEntry{}, map[int]crawlFail{}
	listing := func(key string, v any) {
		b, _ := json.Marshal(v)
		os.WriteFile(filepath.Join(cacheDir, "json", key+".json"), b, 0644)
	}
	metaAt := func(id int, size int64, age time.Duration) {
		writeMeta(id, Meta{Size: size})
		os.Chtimes(metaFile(id), time.Now().Add(-age), time.Now().Add(-age))
	}
	listing("catalog-1", []Item{
		{ID: 1, NameEn: "One", Created: "2026-01-02 00:00:00"}, // new
		{ID: 2, NameEn: "Two", Created: "2026-01-03 00:00:00"}, // new, newer
		{ID: 3, NameEn: "Three"},                               // no source, past refreshTTL
		{ID: 4, NameEn: "Four"},                                // playable, past metaTTL
		{ID: 5, NameEn: "Five"},                                // playable, fresh
		{ID: 6, NameEn: "Six"},                                 // new, but failed recently
	})
	listing("catalog-2", []Item{{ID: 10, NameEn: "Show"}, {ID: 20, NameEn: "Dead show"}, {ID: 30, NameEn: "Unseen"}})
	listing(showKey(10), ShowInfo{Seasons: []int{1, 2}})
	listing(episodesKey(10, 1), []Item{{ID: 11}, {ID: 12, Created: "2026-01-01 00:00:00"}})
	listing(episodesKey(10, 2), []Item{{ID: 13}})
	listing(showKey(20), ShowInfo{Seasons: []int{1}})
	listing(episodesKey(20, 1), []Item{{ID: 21}})
	metaAt(3, 0, 2*time.Hour)
	metaAt(4, 100<<20, 48*time.Hour)
	metaAt(5, 100<<20, 0)
	metaAt(11, 100<<20, 0)
	metaAt(13, 0, 0)
	metaAt(21, 0, 3*time.Hour)
	crawlFailed[6] = crawlFail{Next: time.Now().Add(time.Hour)}
	listing(showKey(30), ShowInfo{Seasons: []int{1}}) // its episodes were never fetched; the crawler fetches them
	slots = make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	var fetches atomic.Int32
	go func() { // the crawler fetches show 30's episodes
		for {
			select {
			case loQ <- struct{}{}:
				fetches.Add(1)
			case <-stop:
				return
			}
		}
	}()
	apiBase, token = "http://127.0.0.1:1", "test" // the fetch fails fast; the crawler goes on

	if got, want := crawlQueue(), []int{2, 1, 12, 21, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	limitMovies = 1
	got := crawlQueue()
	limitMovies = 0
	if want := []int{1, 12, 21}; !slices.Equal(got, want) {
		t.Fatalf("queue with -limit-movies 1: %v, want %v", got, want)
	}
	// Past the listing budget, listings come from the cache only.
	defer func(b time.Duration) { listingBudget = b }(listingBudget)
	listingBudget = 0
	fetches.Store(0)
	if got, want := crawlQueue(), []int{2, 1, 12, 21, 3, 4}; !slices.Equal(got, want) || fetches.Load() != 0 {
		t.Fatalf("queue %v with %d fetches, want %v with none", got, fetches.Load(), want)
	}
}

// A title the crawler keeps failing on waits longer each time, up to crawlMaxWait; a failure every title
// shares (here sloflix's API down) retries in failTTL without growing the wait.
func TestCrawlBackoff(t *testing.T) {
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	crawlFailed, slots = map[int]crawlFail{}, make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	// The title's only source is on a dead video server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"success","data":{"media_sources":[{"media_source":"https://player.sloflix.com/?source=http://127.0.0.1:1/f.mp4","media_source_name":"X"}]}}`))
	}))
	defer srv.Close()
	apiBase, token = srv.URL, "test"
	var waits, want []time.Duration
	for w := time.Hour; w < crawlMaxWait; w *= 2 {
		want = append(want, w)
	}
	want = append(want, crawlMaxWait, crawlMaxWait)
	for range want {
		crawl(1)
		if _, _, ok := due(1); ok {
			t.Fatal("a failed title is due right away")
		}
		waits = append(waits, crawlFailed[1].Wait)
		crawlFailed[1] = crawlFail{Wait: crawlFailed[1].Wait} // its wait is over
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	apiBase = "http://127.0.0.1:1"
	crawl(1)
	if f := crawlFailed[1]; f.Wait != crawlMaxWait || time.Until(f.Next) > failTTL {
		t.Fatalf("after an API failure: wait %v, next in %v; want the wait kept and a retry in %v", f.Wait, time.Until(f.Next), failTTL)
	}
	// A restart keeps the backoff; long-overdue titles (gone from the catalog) are dropped.
	crawlFailed[1] = crawlFail{Next: time.Now(), Wait: crawlMaxWait}
	crawlFailed[2] = crawlFail{Next: time.Now().Add(-crawlMaxWait - time.Hour)}
	saveFailed()
	crawlFailed = map[int]crawlFail{}
	loadFailed()
	if f, ok := crawlFailed[1]; !ok || f.Wait != crawlMaxWait || len(crawlFailed) != 1 {
		t.Fatalf("after a restart: %v", crawlFailed)
	}
}

// A title verified while its meta can't be written (a full disk) isn't listed, so it's retried in failTTL
// instead of counting as checked.
func TestCrawlMetaWriteFails(t *testing.T) {
	video := make([]byte, minVideoSize+1)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.mp4", time.Time{}, bytes.NewReader(video))
	}))
	defer cdn.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"success","data":{"media_sources":[{"media_source":"https://player.sloflix.com/?source=%s/f.mp4","media_source_name":"X"}]}}`, cdn.URL)
	}))
	defer srv.Close()
	cacheDir, apiBase, token = t.TempDir(), srv.URL, "test"
	crawlFailed, slots = map[int]crawlFail{}, make(chan struct{}, 1)
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0555) // unwritable
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	crawl(1)
	if f, ok := crawlFailed[1]; !ok || f.Wait != 0 || time.Until(f.Next) > failTTL {
		t.Fatalf("after a failed meta write: %+v (recorded %v), want a retry in failTTL", f, ok)
	}
}
