// Package cdn fetches from video servers: range requests, size probes, certificate checks and
// remembering which hosts are down.
package cdn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Lenart12/sloflixfs/internal/web"
)

// referer is the player page, which video servers expect.
const referer = "https://player.sloflix.com/"

// Client is the HTTP client for video servers and DoodStream.
var Client = &http.Client{Transport: transport()}

// transport has dial/TLS timeouts (from the default transport), plus a response header timeout.
// No overall timeout: playback responses stream for hours.
func transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	// Healthy video servers connect in ~100ms; a dead one shouldn't hold a lookup for long.
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	// Go's own verification would reject expired certificates before VerifyConnection runs, so it's
	// skipped and verifyCert does the full verification itself.
	t.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyCert(cs, nil) },
	}
	return t
}

// GetRange requests stream URL u from byte off on, unless its video server is remembered as down.
func GetRange(ctx context.Context, u string, off int64) (*http.Response, error) {
	return get(ctx, u, fmt.Sprintf("bytes=%d-", off))
}

func get(ctx context.Context, u, rng string) (*http.Response, error) {
	req, err := web.NewRequest(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("Range", rng)
	return do(req)
}

// RangeSize is the file size in a range response's Content-Range, or 0 if it isn't one.
func RangeSize(resp *http.Response) int64 {
	if resp.StatusCode != http.StatusPartialContent {
		return 0
	}
	_, total, _ := strings.Cut(resp.Header.Get("Content-Range"), "/")
	size, _ := strconv.ParseInt(total, 10, 64)
	return size
}

// do sends req to a video server, failing fast while its host is remembered as down.
func do(req *http.Request) (*http.Response, error) {
	addr := hostPort(req.URL)
	if ok, known := hostStatus(addr); known && !ok {
		return nil, fmt.Errorf("video server %s failed recently, skipped", req.URL.Host)
	}
	resp, err := Client.Do(req)
	hostResult(addr, err)
	return resp, Redact(err)
}

// Redact drops the query, which carries stream tokens, from a request error's URL, so logging the error
// doesn't log a working link.
func Redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery = ""
			ue.URL = u.String()
		}
	}
	return err
}
