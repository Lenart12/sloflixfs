package dood

import (
	"slices"
	"testing"
	"time"
)

// The crawler stops at Max-reserve lookups per window; opens can still use the rest. An open
// waits up to hiWait for a slot (failing at once if none frees up in time), and goes before the crawler.
func TestDoodWindow(t *testing.T) {
	defer func(w, hw time.Duration) { Window, hiWait, sent, lastLo = w, hw, nil, time.Time{} }(Window, hiWait)
	Window, hiWait, lastLo = 500*time.Millisecond, 100*time.Millisecond, time.Time{}
	start := time.Now()
	sent = slices.Repeat([]time.Time{start}, Max-reserve) // the crawler's share, used
	crawled := make(chan struct{})
	go func() { take(false); close(crawled) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-crawled:
		t.Fatal("crawler lookup exceeded its share")
	default:
	}
	for range reserve {
		if err := take(true); err != nil {
			t.Fatalf("open within the reserve: %v", err)
		}
	}
	if err := take(true); err == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("open %d, with no slot freeing within hiWait, should fail at once: %v after %v", Max+1, err, time.Since(start))
	}
	hiWait = time.Second
	if err := take(true); err != nil {
		t.Fatalf("open %d should wait for the window: %v", Max+1, err)
	}
	if waited := time.Since(start); waited < 450*time.Millisecond {
		t.Fatalf("open %d only waited %v", Max+1, waited)
	}
	select {
	case <-crawled:
		t.Fatal("the crawler went before a waiting open")
	default:
	}
	<-crawled
}

// The crawler's lookups are spaced pace apart; playback isn't paced.
func TestDoodPace(t *testing.T) {
	defer func(w time.Duration) { Window, sent, lastLo = w, nil, time.Time{} }(Window)
	Window, sent, lastLo = 1200*time.Millisecond, nil, time.Time{} // pace 100ms
	start := time.Now()
	take(false)
	take(true)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("first crawler lookup or playback waited")
	}
	take(false)
	if waited := time.Since(start); waited < 90*time.Millisecond {
		t.Fatalf("second crawler lookup after %v, want the %v pace", waited, pace())
	}

	// While an open waits for a slot, the crawler holds back.
	mu.Lock()
	hiWaiting++
	mu.Unlock()
	done := make(chan struct{})
	go func() { take(false); close(done) }()
	select {
	case <-done:
		t.Fatal("crawler lookup while an open was waiting")
	case <-time.After(300 * time.Millisecond):
	}
	mu.Lock()
	hiWaiting--
	mu.Unlock()
	<-done
}
