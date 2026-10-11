package sloflix

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
