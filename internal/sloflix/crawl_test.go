package sloflix

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	crawlFailed[6] = crawlFail{next: time.Now().Add(time.Hour)}
	listing(showKey(30), ShowInfo{Seasons: []int{1}}) // its episodes were never fetched; the crawler fetches them
	slots = make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the crawler fetches show 30's episodes
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	apiBase, token = "http://127.0.0.1:1", "test" // the fetch fails fast; the crawler goes on

	if got, want := crawlQueue(0, 0), []int{2, 1, 12, 21, 3, 4}; !slices.Equal(got, want) {
		t.Fatalf("queue %v, want %v", got, want)
	}
	if got, want := crawlQueue(1, 0), []int{1, 12, 21}; !slices.Equal(got, want) {
		t.Fatalf("queue with -limit-movies 1: %v, want %v", got, want)
	}
}

// A title the crawler keeps failing on waits longer each time, up to crawlMaxWait.
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
	apiBase, token = "http://127.0.0.1:1", "test" // every lookup fails
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
		waits = append(waits, crawlFailed[1].wait)
		crawlFailed[1] = crawlFail{wait: crawlFailed[1].wait} // its wait is over
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
}
