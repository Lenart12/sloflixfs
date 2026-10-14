package sloflix

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/dood"
	"github.com/Lenart12/sloflixfs/internal/jellyfin"
	"github.com/Lenart12/sloflixfs/internal/logx"
)

// crawlMaxWait caps the backoff of a title the crawler keeps failing on: failTTL doubled 7 times.
const crawlMaxWait = 128 * time.Hour

type crawlFail struct {
	Next time.Time     // no retry before this
	Wait time.Duration // failTTL, doubled with each failure in a row up to crawlMaxWait
}

// listingBudget is how long a queue rebuild spends refreshing show listings.
var listingBudget = 5 * time.Minute

// failedFile keeps crawlFailed across restarts, so failing titles aren't all retried at once.
func failedFile() string { return filepath.Join(cacheDir, "failed.json") }

func loadFailed() {
	b, _ := os.ReadFile(failedFile())
	lookupMu.Lock()
	json.Unmarshal(b, &crawlFailed)
	lookupMu.Unlock()
}

// saveFailed writes crawlFailed, dropping titles long overdue: they left the catalog.
func saveFailed() {
	lookupMu.Lock()
	maps.DeleteFunc(crawlFailed, func(_ int, f crawlFail) bool { return time.Since(f.Next) > crawlMaxWait })
	b, _ := json.Marshal(crawlFailed)
	lookupMu.Unlock()
	if err := writeFile(failedFile(), b); err != nil {
		logx.Warnf("crawl: saving backoff state: %v", err)
	}
}

// due reports whether the crawler should check id now, and its bucket: 0 for a new title, 1 for one
// without a source or in goneGrace (rechecked after refreshTTL), 2 for a playable one (after metaTTL).
// written is when its meta was last written.
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

// Crawl checks titles in the background forever, so listings only show playable titles and never wait on
// upstream. New titles go first, newest first, then rechecks, least recent first. The extra dood.Max
// workers wait for DoodStream without holding up titles that don't need it.
func Crawl() {
	for {
		go saveHeads()
		queue := crawlQueue()
		saveFailed()
		go jf.Scan()
		// Rebuild every 10 minutes, so new titles don't wait behind a long recheck queue.
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

// crawlQueue refreshes the catalog and returns the titles due for a check, in order.
func crawlQueue() []int {
	var tasks []crawlTask
	add := func(id int, created time.Time) {
		if bucket, written, ok := due(id); ok {
			if bucket == 0 {
				written = created
			}
			tasks = append(tasks, crawlTask{id, bucket, written})
		}
	}
	movies, err := catalog(Movies)
	if err != nil {
		logx.Warnf("crawl: %v", err)
	}
	for _, it := range limit(Movies, movies) {
		add(it.ID, it.CreatedAt())
	}
	shows, err := catalog(Shows)
	if err != nil {
		logx.Warnf("crawl: %v", err)
	}
	// Refreshing all ~1300 show and season listings takes over 20 minutes at 1 request/s, so titles are
	// checked between batches.
	for _, ep := range episodeList(limit(Shows, shows), time.Now().Add(listingBudget)) {
		add(ep.ID, ep.CreatedAt())
	}
	return order(tasks)
}

// episodeList lists the shows' episodes, refreshing listings until deadline and reading the rest cached.
func episodeList(shows []Item, deadline time.Time) []Item {
	var out []Item
	for _, show := range shows {
		si, err := CachedShow(show.ID), error(nil)
		if time.Now().Before(deadline) {
			si, err = showMeta(show.ID)
		}
		if err != nil {
			logx.Warnf("crawl: show %d: %v", show.ID, err)
			continue
		}
		for _, s := range si.Seasons {
			eps := CachedEpisodes(show.ID, s)
			if time.Now().Before(deadline) {
				if eps, err = episodes(show.ID, s); err != nil {
					logx.Warnf("crawl: show %d season %d: %v", show.ID, s, err)
				}
			}
			out = append(out, eps...)
		}
	}
	return out
}

// crawlTask is a title due for a check, with its bucket and when it was created (bucket 0) or last checked.
type crawlTask struct {
	id, bucket int
	at         time.Time
}

// order sorts tasks by bucket, new titles newest first and rechecks least recent first, and returns their
// IDs.
func order(tasks []crawlTask) []int {
	slices.SortFunc(tasks, func(a, b crawlTask) int {
		if a.bucket != b.bucket {
			return a.bucket - b.bucket
		}
		if a.bucket == 0 {
			return b.at.Compare(a.at)
		}
		return a.at.Compare(b.at)
	})
	ids, fresh, retry := make([]int, len(tasks)), 0, 0
	lookupMu.Lock()
	for i, t := range tasks {
		ids[i] = t.id
		if _, failed := crawlFailed[t.id]; t.bucket == 0 && failed {
			retry++ // never verified, such as a title on a dead video server
		} else if t.bucket == 0 {
			fresh++
		}
	}
	lookupMu.Unlock()
	if len(ids) > 0 {
		logx.Infof("crawl: %d new, %d retrying, %d to recheck; %d of %d probe cache slots used", fresh, retry, len(ids)-fresh-retry, headCount(), probeCache)
	}
	return ids
}

// jf scans Jellyfin's libraries once the crawler lists new titles; nil if off.
var jf *jellyfin.Scanner

// newlyListed counts a title for Jellyfin's next scan. It scans now if the probe cache is half full, so new
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
			// A bug: skip the title for crawlMaxWait, even across restarts, rather than crash and unmount.
			logx.Errorf("crawl %d: panic: %v\n%s", id, r, debug.Stack())
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
	_, m, err := resolve(id, background)
	if err == nil && !wasListed {
		logx.Infof("media %d added", id)
		newlyListed()
	}
	backoff(id, err)
	if err != nil {
		if _, listed := Info(id); wasListed && !listed {
			logx.Infof("media %d removed: %v", id, err)
		} else {
			logx.Debugf("crawl %d: %v", id, err)
		}
		return
	}
	revalidateSub(id, m)
}

// backoff records a check's outcome. A title that keeps failing on its own, such as on a dead video server,
// waits twice as long each time. After a failure all titles share, it's retried in failTTL.
func backoff(id int, err error) {
	lookupMu.Lock()
	defer lookupMu.Unlock()
	if err == nil {
		delete(crawlFailed, id)
		return
	}
	f, wait := crawlFailed[id], failTTL
	if !errors.As(err, new(sharedErr)) {
		f.Wait = min(max(2*f.Wait, failTTL), crawlMaxWait)
		wait = f.Wait
	}
	f.Next = time.Now().Add(wait)
	crawlFailed[id] = f
}

// revalidateSub refreshes a title's cached subtitle, bumping its mtime if the subtitle changed.
func revalidateSub(id int, m Meta) {
	if _, err := os.Stat(subFile(m.Sub)); m.Sub == "" || err != nil {
		return
	}
	if changed, err := fetchSubtitle(m.Sub); err != nil {
		logx.Debugf("crawl %d: %v", id, err)
	} else if changed {
		logx.Infof("media %d changed: subtitle %q", id, m.Sub)
		m.Changed = time.Now()
		if err := writeMeta(id, m); err != nil {
			logx.Warnf("media %d: saving meta: %v", id, err)
		}
	}
}
