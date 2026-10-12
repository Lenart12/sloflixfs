package dood

import (
	"fmt"
	"slices"
	"sync"
	"time"
)

// DoodStream allows Max lookups per IP per 5 minutes, then asks for a captcha for about 5 minutes. The
// crawler leaves reserve of them for playback.
const (
	Max     = 15
	reserve = 3
)

var (
	Window = 5*time.Minute + 10*time.Second // DoodStream's 5 minutes plus a margin
	hiWait = time.Minute                    // how long an open waits for a lookup slot

	mu sync.Mutex
	// sent holds the lookups within Window, oldest first. A process that just restarted may have used the
	// limit, so the crawler's share starts used.
	sent      = slices.Repeat([]time.Time{time.Now()}, Max-reserve)
	lastLo    time.Time // the crawler's last lookup
	hiWaiting int       // opens waiting for a slot
)

// pace spaces the crawler's lookups evenly over its share of the window, so it never bursts.
func pace() time.Duration { return Window / (Max - reserve) }

// take waits until a lookup fits the window, then records it. Playback waits at most hiWait, failing at
// once if no slot frees up by then, and the crawler holds back while it waits.
func take(playback bool) error {
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
		wait := waitLocked(now, playback)
		if wait <= 0 {
			sent = append(sent, now)
			if !playback {
				lastLo = now
			}
			mu.Unlock()
			return nil
		}
		if playback && now.Add(wait).After(deadline) {
			mu.Unlock()
			return fmt.Errorf("DoodStream lookup limit reached, free again in %v", wait.Round(time.Second))
		}
		if playback && !waiting {
			hiWaiting++
			waiting = true
		}
		mu.Unlock()
		time.Sleep(wait)
	}
}

// waitLocked drops lookups older than Window and returns how long until the next one fits. mu must be held.
func waitLocked(now time.Time, playback bool) time.Duration {
	for len(sent) > 0 && now.Sub(sent[0]) >= Window {
		sent = sent[1:]
	}
	limit := Max
	if !playback {
		limit -= reserve
	}
	var wait time.Duration
	if len(sent) >= limit {
		wait = sent[len(sent)-limit].Add(Window).Sub(now)
	}
	if playback {
		return wait
	}
	wait = max(wait, lastLo.Add(pace()).Sub(now))
	if hiWaiting > 0 {
		wait = max(wait, time.Second) // an open is waiting: let it go first
	}
	return wait
}

// TakePlayback waits for a lookup slot for an open, failing if none frees up within hiWait.
func TakePlayback() error { return take(true) }

// TakeBackground waits for one of the crawler's lookup slots.
func TakeBackground() error { return take(false) }
