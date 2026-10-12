package sloflix

import (
	"time"
)

// priority orders upstream requests: an open waits on them, the crawler doesn't.
type priority bool

const (
	background priority = false
	playback   priority = true
)

var (
	slots chan struct{} // upstream lookups/fetches in flight, capped by -concurrency; set by Start
	hiQ   = make(chan struct{})
	loQ   = make(chan struct{})
)

// throttleLoop hands out one request slot at a time, at least every apart, preferring waiters on hi
// (playback) over lo. The first slot after an idle period goes out at once, but never two back to back.
func throttleLoop(every time.Duration, hi, lo chan struct{}) {
	for {
		select {
		case hi <- struct{}{}:
		default:
			select {
			case hi <- struct{}{}:
			case lo <- struct{}{}:
			}
		}
		time.Sleep(every)
	}
}

// acquire takes a concurrency slot for a lookup or fetch and returns its release. Take it before waiting for
// the rate limit so ticks aren't spent on requests that can't start. Playback bypasses the limit, so
// slots held by stuck background requests can't stall it; it still waits for the rate limit.
func acquire(p priority) func() {
	if p == playback {
		return func() {}
	}
	slots <- struct{}{}
	return func() { <-slots }
}

// wait takes a sloflix API slot.
func wait(p priority) { take(p, hiQ, loQ) }

func take(p priority, hq, lq chan struct{}) {
	if p == playback {
		<-hq
	} else {
		<-lq
	}
}
