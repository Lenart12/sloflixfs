package dood

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// DoodStream lookups have their own limit: playmogo.com (where every mirror redirects) allows 15 pass_md5
// requests per IP per 5 minutes, then answers with RELOAD and a captcha for ~5 minutes (measured 2026-10-09).
// The crawler leaves reserve of them for playback.
const (
	Max     = 15
	reserve = 3
)

var (
	Window = 5*time.Minute + 10*time.Second // the measured 5 minutes plus margin; a var for tests
	mu     sync.Mutex
	// DoodStream lookups within Window, oldest first; guarded by mu. A previous run (just restarted) may have
	// used the limit, so the crawler's share starts used.
	sent   = slices.Repeat([]time.Time{time.Now()}, Max-reserve)
	lastLo time.Time // the crawler's last lookup; guarded by mu
	// hiWaiting counts opens waiting for a slot; guarded by mu. hiWait bounds that wait: long
	// enough for a scan's probes to queue up, short enough that an open doesn't hang. A var for tests.
	hiWaiting int
	hiWait    = time.Minute
)

// pace spaces the crawler's lookups evenly over the window, at its share of it: 5m10s / (15-3) = 25.8s.
// Bursts of 12 would be just as much within the limit, but leave the crawler idle (and the log silent) for
// minutes, and would be the first thing to break if DoodStream ever penalised bursts.
func pace() time.Duration { return Window / (Max - reserve) }

// take waits until a DoodStream lookup fits the window, then records it. A sliding window of 15 also
// stays within a fixed window or token bucket of that size, whichever DoodStream uses. Playback (any open,
// including Jellyfin's probes) waits at most hiWait, and fails at once if no slot frees up by then;
// while it waits, the crawler holds back, so opens get the whole window.
func take(hi bool) error {
	limit := Max
	if !hi {
		limit -= reserve
	}
	deadline, waiting := time.Now().Add(hiWait), false
	defer func() {
		if waiting {
			mu.Lock()
			hiWaiting--
			mu.Unlock()
		}
	}()
	for {
		mu.Lock()
		now := time.Now()
		for len(sent) > 0 && now.Sub(sent[0]) >= Window {
			sent = sent[1:]
		}
		var wait time.Duration
		if len(sent) >= limit {
			wait = sent[len(sent)-limit].Add(Window).Sub(now)
		}
		if !hi {
			wait = max(wait, lastLo.Add(pace()).Sub(now))
			if hiWaiting > 0 {
				wait = max(wait, time.Second) // an open is waiting: let it go first
			}
		}
		if wait <= 0 {
			sent = append(sent, now)
			if !hi {
				lastLo = now
			}
			mu.Unlock()
			return nil
		}
		if hi && now.Add(wait).After(deadline) {
			mu.Unlock()
			return fmt.Errorf("DoodStream lookup limit reached, free again in %v", wait.Round(time.Second))
		}
		if hi && !waiting {
			hiWaiting++
			waiting = true
		}
		mu.Unlock()
		time.Sleep(wait)
	}
}

// TakePlayback waits for a lookup slot for an open, failing if none frees up within hiWait.
func TakePlayback() error { return take(true) }

// TakeBackground waits for one of the crawler's lookup slots.
func TakeBackground() error { return take(false) }
