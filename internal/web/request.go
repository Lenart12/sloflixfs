// Package web holds the HTTP helpers shared by every upstream client.
package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// UserAgent is a browser's, which Cloudflare in front of sloflix lets through.
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"

// NewRequest builds a request with UserAgent. URLs partly come from upstream data, so an invalid one is an
// error, not a panic.
func NewRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	return req, nil
}

// ReadAll reads r, failing if it holds more than limit bytes.
func ReadAll(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("response over %d bytes", limit)
	}
	return b, err
}
