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

	// fetchMu holds one lock per listing key, so concurrent requests for a listing share one fetch while
	// other listings (and their disk reads) don't wait behind it.
	fetchMu sync.Map // listing key -> *sync.Mutex
)

type memEntry struct {
	v  any
	at time.Time
}

// cached returns fetch()'s result, memoized in memory and on disk for refreshTTL.
// If a refresh fails, the stale copy is served.
func cached[T any](key string, fetch func() (T, error)) (T, error) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	mu, _ := fetchMu.LoadOrStore(key, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	memMu.Lock()
	e, ok = mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	var v T
	file := filepath.Join(cacheDir, "json", key+".json")
	if st, err := os.Stat(file); err == nil {
		b, _ := os.ReadFile(file)
		if json.Unmarshal(b, &v) == nil {
			e, ok = memEntry{v, st.ModTime()}, true
			if time.Since(st.ModTime()) < refreshTTL {
				memMu.Lock()
				mem[key] = e
				memMu.Unlock()
				return v, nil
			}
		}
	}
	release := acquire(false)
	v, err := fetch()
	release()
	memMu.Lock()
	if err == nil && ok && entries(v)*10 < entries(e.v)*9 {
		if first, seen := shrunk[key]; !seen {
			shrunk[key] = time.Now()
			err = fmt.Errorf("%d entries, down from %d; holding the old list for up to %v", entries(v), entries(e.v), shrinkAccept)
		} else if time.Since(first) < shrinkAccept {
			err = fmt.Errorf("%d entries, down from %d since %s", entries(v), entries(e.v), first.Format(time.DateTime))
		} else {
			log.Printf("%s: accepting shrink to %d entries after %v", key, entries(v), shrinkAccept)
		}
	}
	if err == nil {
		delete(shrunk, key)
	}
	memMu.Unlock()
	if err != nil {
		if ok {
			log.Printf("refresh %s failed, serving stale: %v", key, err)
			// Keep serving the stale copy for failTTL instead of retrying on every listing.
			memMu.Lock()
			mem[key] = memEntry{e.v, time.Now().Add(failTTL - refreshTTL)}
			memMu.Unlock()
			return e.v.(T), nil
		}
		return v, err
	}
	b, _ := json.Marshal(v)
	if err := writeFile(file, b); err != nil {
		log.Printf("%s: %v", key, err) // still served from memory
	}
	memMu.Lock()
	mem[key] = memEntry{v, time.Now()}
	memMu.Unlock()
	return v, nil
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

// catalog lists all movies (typ 1) or shows (typ 2).
func catalog(typ int) ([]Item, error) {
	return cached(catalogKey(typ), func() ([]Item, error) {
		var all []Item
		// 300 is the API's max page size.
		for off := 0; ; off += 300 {
			if off >= maxPages*300 {
				return nil, fmt.Errorf("catalog %d: over %d pages", typ, maxPages)
			}
			var page []Item
			if err := api(fmt.Sprintf("/media?sortBy=1&genres=&type=%d&query=&limit=300&offset=%d", typ, off), &page, false); err != nil {
				return nil, err
			}
			all = append(all, page...)
			if len(page) < 300 {
				return all, nil
			}
		}
	})
}

// limit cuts a catalog of typ to its newest -limit-movies or -limit-shows titles, if set.
func limit(typ int, items []Item) []Item {
	n := limitMovies
	if typ == 2 {
		n = limitShows
	}
	if n > 0 {
		return items[:min(n, len(items))]
	}
	return items
}

// peek returns a cached listing from memory or disk, however old, without fetching it.
func peek[T any](key string) (T, bool) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok {
		return e.v.(T), true
	}
	var v T
	b, err := os.ReadFile(filepath.Join(cacheDir, "json", key+".json"))
	return v, err == nil && json.Unmarshal(b, &v) == nil
}

// CachedCatalog returns the catalog of typ (1 movies, 2 shows) from the cache, however old, without
// fetching, so listing the filesystem never waits on upstream. The crawler keeps it fresh. Likewise
// CachedShow and CachedEpisodes.
func CachedCatalog(typ int) []Item { v, _ := peek[[]Item](catalogKey(typ)); return limit(typ, v) }

// CachedShow returns a show's seasons and plot from the cache (see CachedCatalog).
func CachedShow(showID int) ShowInfo { v, _ := peek[ShowInfo](showKey(showID)); return v }

// CachedEpisodes returns a season's episodes from the cache (see CachedCatalog).
func CachedEpisodes(showID, season int) []Item {
	v, _ := peek[[]Item](episodesKey(showID, season))
	return v
}

func catalogKey(typ int) string             { return fmt.Sprintf("catalog-%d", typ) }
func showKey(showID int) string             { return fmt.Sprintf("show-%d", showID) }
func episodesKey(showID, season int) string { return fmt.Sprintf("episodes-%d-%d", showID, season) }

func showMeta(showID int) (ShowInfo, error) {
	return cached(showKey(showID), func() (ShowInfo, error) {
		var d ShowInfo
		err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", showID), &d, false)
		return d, err
	})
}

func episodes(showID, season int) ([]Item, error) {
	return cached(episodesKey(showID, season), func() ([]Item, error) {
		var eps []Item
		err := api(fmt.Sprintf("/media/episodes/%d/%d", showID, season), &eps, false)
		return eps, err
	})
}
