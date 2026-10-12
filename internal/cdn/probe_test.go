package cdn

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeSize(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("error_nofile"))
			return
		}
		w.Header().Set("Content-Range", "bytes 0-0/12345")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	var conns atomic.Int32
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	for range 2 {
		if n, err := ProbeSize(srv.URL + "/ok"); err != nil || n != 12345 {
			t.Fatalf("ok: %d %v", n, err)
		}
	}
	if conns.Load() != 1 {
		t.Fatalf("probe connections not reused: %d", conns.Load())
	}
	if _, err := ProbeSize(srv.URL + "/gone"); !errors.Is(err, ErrFileGone) {
		t.Fatalf("gone: %v", err)
	}

	// A video server that refused two connections in a row is skipped without another attempt.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	dead.Close()
	for i := range 2 {
		if _, err := ProbeSize("http://" + dead.Addr().String() + "/f"); err == nil || strings.Contains(err.Error(), "skipped") {
			t.Fatalf("probe %d of a dead server should try to connect: %v", i+1, err)
		}
	}
	if _, err := ProbeSize("http://" + dead.Addr().String() + "/f"); err == nil || !strings.Contains(err.Error(), "skipped") {
		t.Fatalf("third probe of a dead server should be skipped: %v", err)
	}
	hosts = map[string]hostCheck{}
	if _, err := ProbeSize("http://" + dead.Addr().String() + "/f?token=secret"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("a failed request's error must not carry the link's token: %v", err)
	}
}
