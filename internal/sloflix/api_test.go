package sloflix

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// An expired token is replaced by one login and the request retried; a failed login isn't retried with
// every API call.
func TestLogin(t *testing.T) {
	var logins atomic.Int32
	var reject atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/login" {
			logins.Add(1)
			if reject.Load() {
				w.Write([]byte(`{"status":"error","metadata":{}}`))
				return
			}
			w.Write([]byte(`{"status":"success","metadata":{"access_token":"new"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer new" {
			w.Write([]byte(`{"status":"error","error":{"message":"jwt expired"}}`))
			return
		}
		w.Write([]byte(`{"status":"success","data":42}`))
	}))
	defer srv.Close()
	cacheDir, apiBase, token = t.TempDir(), srv.URL, "expired"
	defer func() { token, loginErr = "", nil }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()

	var v int
	if err := api("/x", &v, false); err != nil || v != 42 || logins.Load() != 1 {
		t.Fatalf("expired token: v=%d err=%v logins=%d", v, err, logins.Load())
	}
	token, cacheDir = "", t.TempDir() // no token anywhere
	reject.Store(true)
	for range 3 {
		if err := api("/x", &v, false); err == nil {
			t.Fatal("api succeeded without a token")
		}
	}
	if n := logins.Load(); n != 2 {
		t.Fatalf("%d logins, want 2 (one each)", n)
	}
}

// The catalog is fetched in pages of 300 until a short one, and a server that never sends one is cut off.
func TestCatalogPaging(t *testing.T) {
	var endless atomic.Bool
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		n := 300
		if off >= 300 && !endless.Load() {
			n = 1
		}
		items := make([]Item, n)
		for i := range items {
			items[i].ID = off + i
		}
		b, _ := json.Marshal(items)
		fmt.Fprintf(w, `{"status":"success","data":%s}`, b)
	}))
	defer srv.Close()
	cacheDir, apiBase, token = t.TempDir(), srv.URL, "test"
	os.MkdirAll(filepath.Join(cacheDir, "json"), 0755)
	mem, slots, refreshTTL = map[string]memEntry{}, make(chan struct{}, 1), time.Hour
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case loQ <- struct{}{}:
			case <-stop:
				return
			}
		}
	}()
	if items, err := catalog(1); err != nil || len(items) != 301 || items[300].ID != 300 {
		t.Fatalf("catalog: %d items, err %v", len(items), err)
	}
	endless.Store(true)
	pages.Store(0)
	if _, err := catalog(2); err == nil || pages.Load() != maxPages {
		t.Fatalf("endless catalog: err %v after %d pages", err, pages.Load())
	}
}
