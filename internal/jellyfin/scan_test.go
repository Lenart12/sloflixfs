package jellyfin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestJellyfinScan(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	state, status := "Idle", "Idle"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != `MediaBrowser Token="key"` {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/ScheduledTasks":
			fmt.Fprintf(w, `[{"Key":"RefreshLibrary","State":%q}]`, state)
		case "/Library/VirtualFolders":
			fmt.Fprintf(w, `[{"Name":"Movies","ItemId":"m","Locations":["/media/sloflix/library/Movies"],"RefreshStatus":%q},
				{"Name":"Shows","ItemId":"s","Locations":["/media/sloflix/library/Shows"],"RefreshStatus":"Idle"},
				{"Name":"Home","ItemId":"h","Locations":["/media/sloflix/library2"],"RefreshStatus":"Idle"}]`, status)
		default:
			posted = append(posted, r.Method+" "+r.URL.Path)
		}
	}))
	defer srv.Close()
	s := New(srv.URL, "key", "/media/sloflix/library")
	scan := func(last time.Time) []string {
		s.mu.Lock()
		s.pending, s.last = 1, last
		s.mu.Unlock()
		mu.Lock()
		posted = nil
		mu.Unlock()
		s.Scan()
		mu.Lock()
		defer mu.Unlock()
		return posted
	}

	if got := scan(time.Time{}); !slices.Equal(got, []string{"POST /Items/m/Refresh", "POST /Items/s/Refresh"}) || s.pending != 0 {
		t.Fatalf("scan posted %v, pending %d", got, s.pending)
	}
	if got := scan(time.Now()); got != nil {
		t.Fatalf("scan within the cooldown posted %v", got)
	}
	status = "Queued"
	if got := scan(time.Time{}); got != nil {
		t.Fatalf("scan while a library refreshes posted %v", got)
	}
	status, state = "Idle", "Running"
	if got := scan(time.Time{}); got != nil {
		t.Fatalf("scan during a full scan posted %v", got)
	}
}
