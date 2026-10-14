package sloflix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Lenart12/sloflixfs/internal/logx"
	"github.com/Lenart12/sloflixfs/internal/web"
)

const (
	subBase = "https://sloflix.com/subtitles/"
	// maxSubtitle bounds a subtitle file, far above the real ~0.1 MB.
	maxSubtitle = 10 << 20
)

// subFile is where subtitle loc is cached, named by its hash: loc comes from upstream, and subtitles in
// different folders can share a file name.
func subFile(loc string) string {
	sum := sha256.Sum256([]byte(loc))
	return filepath.Join(cacheDir, "subtitles", hex.EncodeToString(sum[:16]))
}

// Subtitle returns subtitle file loc, from the cache or downloaded.
func Subtitle(loc string) ([]byte, error) {
	if b, err := os.ReadFile(subFile(loc)); err == nil {
		return b, nil
	}
	if _, err := fetchSubtitle(loc); err != nil {
		return nil, err
	}
	return os.ReadFile(subFile(loc))
}

// fetchSubtitle downloads loc into the cache, or revalidates the cached copy by ETag.
// It reports whether the cached content changed.
func fetchSubtitle(loc string) (bool, error) {
	defer acquire(background)()
	wait(background)
	logx.Debugf("sub: GET %q", loc)
	req, err := web.NewRequest(context.Background(), "GET", subBase+url.PathEscape(loc), nil)
	if err != nil {
		return false, err
	}
	if etag, err := os.ReadFile(subFile(loc) + ".etag"); err == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("subtitle %q: %s", loc, resp.Status)
	}
	b, err := web.ReadAll(resp.Body, maxSubtitle)
	if err != nil {
		return false, fmt.Errorf("subtitle %q: %w", loc, err)
	}
	old, _ := os.ReadFile(subFile(loc))
	if err := writeFile(subFile(loc), b); err != nil {
		return false, err
	}
	// After the content: a new ETag with the old content would keep it stale for good.
	return !bytes.Equal(old, b), writeFile(subFile(loc)+".etag", []byte(resp.Header.Get("ETag")))
}
