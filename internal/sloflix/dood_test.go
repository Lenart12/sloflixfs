package sloflix

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/dood"
)

// sloflix spells the source name inconsistently ("DoodStream", "Doodstream"); both must use the embed fallback.
func TestResolveDoodSpelling(t *testing.T) {
	var size atomic.Int64
	size.Store(777777777)
	var head atomic.Value
	head.Store("x")
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/e/abc":
			w.Write([]byte(`<script>$.get('/pass_md5/1-2/tok', function(d){})</script>`))
		case strings.HasPrefix(r.URL.Path, "/pass_md5/"):
			w.Write([]byte(srv.URL + "/file~"))
		case strings.HasPrefix(r.URL.Path, "/file~"):
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size.Load()))
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte(head.Load().(string)))
		case r.URL.Path == "/media/single/10": // a source URL that doesn't parse (title 9695 crashed resolve)
			fmt.Fprintf(w, `{"status":"success","data":{"media_sources":[{"media_source":"https://x/%%zz","media_source_name":"X"}]}}`)
		default: // the sloflix API
			fmt.Fprintf(w, `{"status":"success","data":{"media_sources":[{"media_source":"https://do7go.com/e/abc","media_source_name":"SLOSubs (Doodstream)"}]}}`)
		}
	}))
	defer srv.Close()
	defer func(c, api *http.Client) { cdn.Client, apiClient = c, api }(cdn.Client, apiClient)
	cdn.Client, apiClient = srv.Client(), srv.Client()
	apiBase, dood.Mirror = srv.URL, srv.URL+"/e/"
	cacheDir = t.TempDir()
	os.MkdirAll(filepath.Join(cacheDir, "meta"), 0755)
	token, slots = "test", make(chan struct{}, 1)
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
	if _, m, err := resolve(7, background); err != nil || m.Size != 777777777 {
		t.Fatalf("size=%d err=%v", m.Size, err)
	}
	// A file under minVideoSize is a broken upload: hidden like a title without a source.
	size.Store(65536)
	if _, _, err := resolve(8, background); err == nil || !strings.Contains(err.Error(), "broken upload") {
		t.Fatalf("tiny file: %v", err)
	}
	if _, ok := Info(8); ok {
		t.Fatal("tiny file is listed")
	}
	// So is a file that isn't a video container.
	size.Store(777777777)
	head.Store("\x14\xd3J(\x88\xabC\xe0DM\"\x05")
	if _, _, err := resolve(11, background); err == nil || !strings.Contains(err.Error(), "not a readable video") {
		t.Fatalf("broken file: %v", err)
	}
	if _, ok := Info(11); ok {
		t.Fatal("broken file is listed")
	}
	writeMeta(9, Meta{Size: 65536}) // recorded as playable before minVideoSize existed
	if _, ok := Info(9); ok {
		t.Fatal("tiny file recorded earlier is listed")
	}
	if _, _, err := resolve(10, background); err == nil || !strings.Contains(err.Error(), "no direct source") {
		t.Fatalf("bad source URL: %v", err)
	}
}
