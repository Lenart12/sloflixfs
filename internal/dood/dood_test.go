package dood

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestDoodURL(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/e/good":
			w.Write([]byte(`<script>$.get('/pass_md5/123-abc/tok42', function(d){})</script>`))
		case "/e/gone":
			w.Write([]byte(`<title>Video not found | DoodStream</title>`))
		case "/e/challenge":
			w.Write([]byte(`<title>Just a moment...</title>`))
		case "/e/encoding": // the placeholder's script shows "Video encoding error."
			w.Write([]byte(`<title>X - DoodStream</title><script>$.get('/dood?op=enc_status&id='+fid,function(){},'script');</script>`))
		case "/e/captcha":
			w.Write([]byte(`<title>X - DoodStream.com</title><script src="//challenges.cloudflare.com/turnstile/v0/api.js"></script>`))
		case "/pass_md5/123-abc/tok42":
			if r.Referer() != srv.URL+"/e/good" {
				t.Errorf("pass_md5 referer %q", r.Referer())
			}
			w.Write([]byte("https://cdn.example/path/file~"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	Mirror = srv.URL + "/e/"
	takes := 0
	noWait := func() error { takes++; return nil }

	u, err := URL("good", noWait)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^https://cdn\.example/path/file~[A-Za-z0-9]{10}\?token=tok42&expiry=\d{13}$`).MatchString(u) {
		t.Fatalf("bad url %q", u)
	}
	if _, err := URL("gone", noWait); !errors.Is(err, ErrGone) {
		t.Fatalf("gone: %v", err)
	}
	if _, err := URL("challenge", noWait); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("challenge page should be a transient error, got %v", err)
	}

	if _, err := URL("captcha", noWait); err == nil || errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "captcha") {
		t.Fatalf("captcha should be a transient captcha error, got %v", err)
	}
	if _, err := URL("encoding", noWait); !errors.Is(err, ErrEncoding) {
		t.Fatalf("encoding: %v", err)
	}
	// Only the working lookup reached pass_md5, the one request DoodStream counts.
	if takes != 1 {
		t.Fatalf("%d lookups took a DoodStream slot, want 1", takes)
	}
}
