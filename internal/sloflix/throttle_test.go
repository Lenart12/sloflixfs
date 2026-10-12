package sloflix

import (
	"os"
	"testing"
	"time"

	"github.com/Lenart12/sloflixfs/internal/dood"
)

func TestMain(m *testing.M) {
	dood.Window = time.Microsecond // tests resolving DoodStream embeds don't wait for the window
	os.Exit(m.Run())
}

// Slots are never handed out closer than the interval, not even right after an idle period.
func TestThrottleSpacing(t *testing.T) {
	hi, lo := make(chan struct{}), make(chan struct{})
	go throttleLoop(50*time.Millisecond, hi, lo)
	time.Sleep(120 * time.Millisecond) // idle
	prev := time.Now()
	<-lo
	for i := range 3 {
		take(i == 1, hi, lo)
		if gap := time.Since(prev); gap < 45*time.Millisecond {
			t.Fatalf("slot %d only %v after the previous one", i+2, gap)
		}
		prev = time.Now()
	}
}

func TestThrottlePrefersHi(t *testing.T) {
	hi, lo := make(chan struct{}), make(chan struct{}) // not hiQ/loQ: a loop left running would hand out ticks to later tests
	go throttleLoop(20*time.Millisecond, hi, lo)
	done := make(chan string, 6)
	for range 5 {
		go func() { take(false, hi, lo); done <- "lo" }()
	}
	time.Sleep(5 * time.Millisecond)
	go func() { take(true, hi, lo); done <- "hi" }()
	for i := range 6 {
		if <-done == "hi" {
			if i > 1 {
				t.Fatalf("hi served at position %d behind queued lo waiters", i)
			}
			return
		}
	}
}
