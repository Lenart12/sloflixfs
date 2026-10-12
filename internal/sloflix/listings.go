package sloflix

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// A listing that shrinks by more than 10% is treated as an upstream glitch, so Jellyfin doesn't delete
	// the missing titles. Only a shrink that persists this long is accepted.
	shrinkAccept = 24 * time.Hour
	// maxPages bounds a catalog, far above the real ~20 pages.
	maxPages = 200
)

var (
	memMu  sync.Mutex
	mem    = map[string]memEntry{}
	shrunk = map[string]time.Time{} // listing key -> when it was first seen shrunk

	// fetchMu holds a *sync.Mutex per listing key, so concurrent requests for a listing share one fetch
	// without blocking other listings.
	fetchMu sync.Map
)

type memEntry struct {
	v  any
	at time.Time
}

// cached returns fetch()'s result, memoized in memory and on disk for refreshTTL.
// If a refresh fails, the stale copy is served.
func cached[T any](key string, fetch func() (T, error)) (T, error) {
	if e, ok := memGet(key); ok && e.fresh() {
		return e.v.(T), nil
	}
	mu, _ := fetchMu.LoadOrStore(key, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	e, ok := memGet(key)
	if ok && e.fresh() {
		return e.v.(T), nil
	}
	if de, found := readCached[T](key); found {
		e, ok = de, true
		if e.fresh() {
			memPut(key, e)
			return e.v.(T), nil
		}
	}
	release := acquire(background)
	v, err := fetch()
	release()
	if err == nil {
		err = checkShrink(key, entries(e.v), entries(v))
	}
	if err != nil {
		if !ok {
			return v, err
		}
		log.Printf("refresh %s failed, serving stale: %v", key, err)
		// Keep serving the stale copy for failTTL instead of retrying on every listing.
		memPut(key, memEntry{e.v, time.Now().Add(failTTL - refreshTTL)})
		return e.v.(T), nil
	}
	b, _ := json.Marshal(v)
	if err := writeFile(cacheFile(key), b); err != nil {
		log.Printf("%s: %v", key, err) // still served from memory
	}
	memPut(key, memEntry{v, time.Now()})
	return v, nil
}

func (e memEntry) fresh() bool { return time.Since(e.at) < refreshTTL }

func memGet(key string) (memEntry, bool) {
	memMu.Lock()
	defer memMu.Unlock()
	e, ok := mem[key]
	return e, ok
}

func memPut(key string, e memEntry) {
	memMu.Lock()
	mem[key] = e
	memMu.Unlock()
}

func cacheFile(key string) string { return filepath.Join(cacheDir, "json", key+".json") }

// readCached reads a listing from disk, dated by its file's mtime.
func readCached[T any](key string) (memEntry, bool) {
	st, err := os.Stat(cacheFile(key))
	if err != nil {
		return memEntry{}, false
	}
	b, _ := os.ReadFile(cacheFile(key))
	var v T
	if json.Unmarshal(b, &v) != nil {
		return memEntry{}, false
	}
	return memEntry{v, st.ModTime()}, true
}

// checkShrink rejects a listing that shrank from old to n entries by more than 10%, until it has stayed
// shrunk for shrinkAccept.
func checkShrink(key string, old, n int) error {
	memMu.Lock()
	defer memMu.Unlock()
	if n*10 >= old*9 {
		delete(shrunk, key)
		return nil
	}
	first, seen := shrunk[key]
	if !seen {
		shrunk[key] = time.Now()
		return fmt.Errorf("%d entries, down from %d; holding the old list for up to %v", n, old, shrinkAccept)
	}
	if time.Since(first) < shrinkAccept {
		return fmt.Errorf("%d entries, down from %d since %s", n, old, first.Format(time.DateTime))
	}
	log.Printf("%s: accepting shrink to %d entries after %v", key, n, shrinkAccept)
	delete(shrunk, key)
	return nil
}

// entries counts a cached listing's entries for the shrink guard.
func entries(v any) int {
	switch v := v.(type) {
	case []Item:
		return len(v)
	case ShowInfo:
		return len(v.Seasons)
	}
	return 0
}

// Kind is a catalog's media type, as the API numbers it.
type Kind int

const (
	Movies Kind = 1
	Shows  Kind = 2
)

// pageSize is the API's largest catalog page.
const pageSize = 300

// catalog lists all titles of kind.
func catalog(kind Kind) ([]Item, error) {
	return cached(catalogKey(kind), func() ([]Item, error) {
		var all []Item
		for off := 0; ; off += pageSize {
			if off >= maxPages*pageSize {
				return nil, fmt.Errorf("catalog %d: over %d pages", kind, maxPages)
			}
			var page []Item
			if err := api(fmt.Sprintf("/media?sortBy=1&genres=&type=%d&query=&limit=%d&offset=%d", kind, pageSize, off), &page, background); err != nil {
				return nil, err
			}
			all = append(all, page...)
			if len(page) < pageSize {
				return all, nil
			}
		}
	})
}

// limit cuts a catalog of kind to its newest -limit-movies or -limit-shows titles, if set.
func limit(kind Kind, items []Item) []Item {
	n := limitMovies
	if kind == Shows {
		n = limitShows
	}
	if n > 0 {
		return items[:min(n, len(items))]
	}
	return items
}

// peek returns a cached listing from memory or disk, however old, without fetching it.
func peek[T any](key string) (T, bool) {
	e, ok := memGet(key)
	if !ok {
		e, ok = readCached[T](key)
	}
	if !ok {
		var zero T
		return zero, false
	}
	return e.v.(T), true
}

// CachedCatalog returns the catalog of kind from the cache, however old, so listing the filesystem never
// waits on upstream. The crawler keeps it fresh.
func CachedCatalog(kind Kind) []Item { v, _ := peek[[]Item](catalogKey(kind)); return limit(kind, v) }

// CachedShow returns a show's seasons and plot from the cache, however old.
func CachedShow(showID int) ShowInfo { v, _ := peek[ShowInfo](showKey(showID)); return v }

// CachedEpisodes returns a season's episodes from the cache, however old.
func CachedEpisodes(showID, season int) []Item {
	v, _ := peek[[]Item](episodesKey(showID, season))
	return v
}

func catalogKey(kind Kind) string           { return fmt.Sprintf("catalog-%d", kind) }
func showKey(showID int) string             { return fmt.Sprintf("show-%d", showID) }
func episodesKey(showID, season int) string { return fmt.Sprintf("episodes-%d-%d", showID, season) }

func showMeta(showID int) (ShowInfo, error) {
	return cached(showKey(showID), func() (ShowInfo, error) {
		var d ShowInfo
		err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", showID), &d, background)
		return d, err
	})
}

func episodes(showID, season int) ([]Item, error) {
	return cached(episodesKey(showID, season), func() ([]Item, error) {
		var eps []Item
		err := api(fmt.Sprintf("/media/episodes/%d/%d", showID, season), &eps, background)
		return eps, err
	})
}
