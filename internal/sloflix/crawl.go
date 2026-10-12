package sloflix

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/dood"
	"github.com/Lenart12/sloflixfs/internal/jellyfin"
)

// crawlMaxWait caps the backoff of a title the crawler keeps failing on (mostly dead CDN servers): failTTL
// doubled with each failure in a row, 7 times.
const crawlMaxWait = 128 * time.Hour

type crawlFail struct {
	Next time.Time     // no retry before this
	Wait time.Duration // failTTL, doubled with each failure in a row up to crawlMaxWait
}

// listingBudget is how long a queue rebuild spends fetching show listings (see crawlQueue). A var for tests.
var listingBudget = 5 * time.Minute

// failedFile keeps crawlFailed across restarts, so titles on dead video servers aren't all retried at once.
func failedFile() string { return filepath.Join(cacheDir, "failed.json") }

func loadFailed() {
	b, _ := os.ReadFile(failedFile())
	lookupMu.Lock()
	json.Unmarshal(b, &crawlFailed)
	lookupMu.Unlock()
}

// saveFailed writes crawlFailed, dropping titles not retried long after they were due: gone from the catalog.
func saveFailed() {
	lookupMu.Lock()
	maps.DeleteFunc(crawlFailed, func(_ int, f crawlFail) bool { return time.Since(f.Next) > crawlMaxWait })
	b, _ := json.Marshal(crawlFailed)
	lookupMu.Unlock()
	if err := writeFile(failedFile(), b); err != nil {
		log.Printf("crawl: %v", err)
	}
}

// due reports whether the crawler should check id now, and in which order: bucket 0 is never checked,
// 1 a title without a source or in its goneGrace period (rechecked after refreshTTL), 2 a playable one
// (rechecked after metaTTL). written is when its meta was last written.
func due(id int) (bucket int, written time.Time, ok bool) {
	lookupMu.Lock()
	recent := time.Now().Before(crawlFailed[id].Next)
	lookupMu.Unlock()
	if recent {
		return 0, time.Time{}, false
	}
	m, written, known := readMeta(id)
	switch {
	case !known:
		return 0, written, true
	case m.Size == 0 || !m.Gone.IsZero():
		return 1, written, time.Since(written) >= refreshTTL
	default:
		return 2, written, time.Since(written) >= metaTTL
	}
}

// Crawl checks titles in the background, forever, so listings only show playable titles and never wait on
// upstream: new titles first (newest first), then rechecks (see due), oldest first. This also catches
// changed or removed sources and subtitles added or edited upstream for titles nobody plays. The rate
// limits pace it; DoodStream's leaves a reserve for playback (dood.TakeBackground). Workers beyond -concurrency (which
// still caps requests in flight) can wait for DoodStream without holding up titles that don't need it.
func Crawl() {
	for {
		go saveHeads()
		queue := crawlQueue()
		saveFailed() // the previous round's failures
		go jf.Scan() // at most hourly, and only after titles were newly listed
		// Rebuild at most every 10 minutes, so newly listed titles don't wait behind a long recheck queue.
		next := time.Now().Add(10 * time.Minute)
		ids := make(chan int)
		var wg sync.WaitGroup
		for range cap(slots) + dood.Max {
			wg.Go(func() {
				for id := range ids {
					crawl(id)
				}
			})
		}
		for _, id := range queue {
			if time.Now().After(next) {
				break
			}
			ids <- id
		}
		close(ids)
		wg.Wait()
		time.Sleep(time.Until(next))
	}
}

// crawlQueue walks the catalog (the only place its listings are refreshed; readdir serves them cached) and
// returns the titles due for a check, in order.
func crawlQueue() []int {
	type task struct {
		id, bucket int
		at         time.Time
	}
	var tasks []task
	add := func(id int, created time.Time) {
		if bucket, written, ok := due(id); ok {
			if bucket == 0 {
				written = created
			}
			tasks = append(tasks, task{id, bucket, written})
		}
	}
	movies, err := catalog(1)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	for _, it := range limit(1, movies) {
		add(it.ID, it.CreatedAt())
	}
	shows, err := catalog(2)
	if err != nil {
		log.Printf("crawl: %v", err)
	}
	// Show and season listings are fetched for at most listingBudget per rebuild, newest shows first; the
	// rest come from the cache (or wait, if never fetched) until a later rebuild. So with an empty cache, or
	// when many expire at once, titles are checked between batches of the ~1500 listings, not after all.
	deadline := time.Now().Add(listingBudget)
	for _, show := range limit(2, shows) {
		si, err := CachedShow(show.ID), error(nil)
		if time.Now().Before(deadline) {
			si, err = showMeta(show.ID)
		}
		if err != nil {
			log.Printf("crawl: show %d: %v", show.ID, err)
			continue
		}
		for _, s := range si.Seasons {
			eps := CachedEpisodes(show.ID, s)
			if time.Now().Before(deadline) {
				if eps, err = episodes(show.ID, s); err != nil {
					log.Printf("crawl: show %d season %d: %v", show.ID, s, err)
				}
			}
			for _, ep := range eps {
				add(ep.ID, ep.CreatedAt())
			}
		}
	}
	slices.SortFunc(tasks, func(a, b task) int {
		if a.bucket != b.bucket {
			return a.bucket - b.bucket
		}
		if a.bucket == 0 {
			return b.at.Compare(a.at) // newest first
		}
		return a.at.Compare(b.at) // least recently checked first
	})
	ids, fresh := make([]int, len(tasks)), 0
	for i, t := range tasks {
		ids[i] = t.id
		if t.bucket == 0 {
			fresh++
		}
	}
	if len(ids) > 0 {
		log.Printf("crawl: %d new, %d to recheck; %d of %d probe cache slots used", fresh, len(ids)-fresh, headCount(), probeCache)
	}
	return ids
}

// jf scans Jellyfin's libraries once the crawler lists new titles; nil if off.
var jf *jellyfin.Scanner

// newlyListed counts a title for Jellyfin's next scan, and scans now if the probe cache is half full, so new
// titles keep getting heads.
func newlyListed() {
	if jf == nil {
		return
	}
	jf.Add()
	if probeCache > 0 && headCount()*2 >= probeCache {
		go jf.Scan()
	}
}

// crawl checks one title, unless playback did meanwhile, and revalidates its cached subtitle.
func crawl(id int) {
	defer func() {
		if r := recover(); r != nil {
			// A bug: keep the title out for crawlMaxWait, also across restarts, rather than crash (unmounting
			// the library) and hit it again after every restart.
			log.Printf("crawl %d: panic: %v\n%s", id, r, debug.Stack())
			lookupMu.Lock()
			crawlFailed[id] = crawlFail{Next: time.Now().Add(crawlMaxWait), Wait: crawlMaxWait}
			lookupMu.Unlock()
			saveFailed()
		}
	}()
	if _, _, ok := due(id); !ok {
		return
	}
	_, wasListed := Info(id)
	_, m, err := resolve(id, false)
	if err == nil && !wasListed { // resolve succeeds only for a playable title: it's newly listed
		newlyListed()
	}
	lookupMu.Lock()
	if err != nil {
		// Back off titles that keep failing on their own (a dead video server can stay dead for days, a
		// missing source rarely comes back); after a shared failure just retry in failTTL.
		f, wait := crawlFailed[id], failTTL
		if !errors.As(err, new(sharedErr)) {
			f.Wait = min(max(2*f.Wait, failTTL), crawlMaxWait)
			wait = f.Wait
		}
		f.Next = time.Now().Add(wait)
		crawlFailed[id] = f
	} else {
		delete(crawlFailed, id)
	}
	lookupMu.Unlock()
	if err != nil {
		log.Printf("crawl %d: %v", id, err)
		return
	}
	if _, err := os.Stat(subFile(m.Sub)); m.Sub == "" || err != nil {
		return
	}
	if changed, err := fetchSubtitle(m.Sub); err != nil {
		log.Printf("crawl %d: %v", id, err)
	} else if changed {
		log.Printf("media %d: subtitle %q changed", id, m.Sub)
		m.Changed = time.Now()
		if err := writeMeta(id, m); err != nil {
			log.Printf("crawl %d: %v", id, err)
		}
	}
}
