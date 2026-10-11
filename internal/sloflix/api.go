package sloflix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var startTime = time.Now()

var apiBase = "https://api.sloflix.com/v1" // a var for tests

const (
	subBase   = "https://sloflix.com/subtitles/"
	userAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"
	referer   = "https://player.sloflix.com/"
	// Stream links expire after 7.5–8h (measured 2026-10: 5 links, direct and embed, all valid at 7h30m and
	// "error_expired" at 8h); a rejected link is re-resolved anyway.
	urlTTL  = 7*time.Hour + 30*time.Minute
	failTTL = time.Hour
	// A failed login (bad credentials, sloflix down) is retried after this.
	loginRetry = 5 * time.Minute
	// A title the crawler keeps failing on (mostly dead CDN servers) waits failTTL, doubling with each
	// failure in a row up to this.
	crawlMaxWait = 128 * time.Hour // 1h doubled 7 times, about 5 days
	// A listing that shrinks by more than 10% is treated as an upstream glitch, so Jellyfin doesn't delete
	// the missing titles. Only a shrink that persists this long is accepted.
	shrinkAccept = 24 * time.Hour
	// A title that used to play stays listed this long after upstream first says it's gone, so one wrong
	// "gone" (an anti-bot page, a CDN hiccup) can't make Jellyfin drop it and its watch history.
	goneGrace = 24 * time.Hour
	// Playback failures are remembered this long, so a probe retrying a dead title doesn't refetch each time.
	playFailTTL = 5 * time.Minute
	// Upper bounds on what upstream sends, far above real sizes (catalog pages ~0.2 MB, subtitles ~0.1 MB,
	// player pages ~0.1 MB, ~20 catalog pages), so a broken or hostile server can't exhaust memory.
	maxJSON, maxSubtitle, maxPage = 64 << 20, 10 << 20, 4 << 20
	maxPages                      = 200
)

// Outcomes that were logged when first determined; callers don't log them again on every listing.
var (
	errNoSource       = errors.New("no direct source")
	errFailedRecently = errors.New("lookup failed recently")
)

// Quiet reports whether err is one of those outcomes.
func Quiet(err error) bool { return errors.Is(err, errNoSource) || errors.Is(err, errFailedRecently) }

var (
	cacheDir   string
	refreshTTL time.Duration
	metaTTL    time.Duration
	probeCache int // -probe-cache: max saved starts of new titles awaiting Jellyfin's probe (saveHead); 0 disables
	username   string
	password   string

	apiClient = &http.Client{Timeout: 30 * time.Second}
	cdnClient = &http.Client{Transport: cdnTransport()}

	tokenMu  sync.Mutex
	token    string
	loginErr error     // the last login's error; guarded by tokenMu
	loginAt  time.Time // when it failed; guarded by tokenMu

	memMu sync.Mutex
	mem   = map[string]memEntry{}

	// fetchMu holds one lock per listing key, so concurrent requests for a listing share one fetch while
	// other listings (and their disk reads) don't wait behind it.
	fetchMu sync.Map                 // listing key -> *sync.Mutex
	shrunk  = map[string]time.Time{} // listing key -> when it was first seen shrunk; guarded by memMu

	lookupMu    sync.Mutex
	failed      = map[int]time.Time{} // failed playback lookups; guarded by lookupMu
	crawlFailed = map[int]crawlFail{} // failed crawler checks, kept apart so they don't block playback; guarded by lookupMu

	slots chan struct{} // upstream lookups/fetches in flight, capped by -concurrency; set by Start

	hiQ = make(chan struct{})
	loQ = make(chan struct{})
)

type memEntry struct {
	v  any
	at time.Time
}

// Item is a movie, show or episode from a catalog or season listing.
type Item struct {
	ID      int      `json:"media_id"`
	Name    string   `json:"media_name"`
	NameEn  string   `json:"media_name_en"`
	Year    int      `json:"media_year"`
	Created string   `json:"created_at"`
	Episode int      `json:"episode_index"`
	Genres  []string `json:"media_genres"`
	Poster  string   `json:"media_thumbnail_url"`
	Banner  string   `json:"media_banner_url"`
}

// CreatedAt is when the title was added to sloflix, or the process start if upstream sent no valid time.
func (it Item) CreatedAt() time.Time {
	t, err := time.Parse(time.DateTime, it.Created)
	if err != nil {
		return startTime
	}
	return t
}

// ShowInfo is a show's seasons and plot.
type ShowInfo struct {
	Seasons []int
	Plot    string `json:"media_description"`
}

// newRequest builds a request with the browser User-Agent. URLs partly come from upstream data, so an
// invalid one is an error, not a panic.
func newRequest(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	return req, nil
}

// login returns the API token: cached in memory or on disk, or from logging in. A failed login is
// remembered for loginRetry, so wrong credentials don't send a login request with every API call.
func login(hi bool) (_ string, err error) {
	tokenMu.Lock()
	defer tokenMu.Unlock()
	if token != "" {
		return token, nil
	}
	if b, err := os.ReadFile(filepath.Join(cacheDir, "token")); err == nil {
		token = string(b)
		return token, nil
	}
	if loginErr != nil && time.Since(loginAt) < loginRetry {
		return "", loginErr
	}
	defer func() { loginErr, loginAt = err, time.Now() }()
	wait(hi)
	log.Printf("api: login")
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := newRequest(context.Background(), "POST", apiBase+"/user/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var r struct {
		Status   string
		Metadata struct {
			AccessToken string `json:"access_token"`
		}
	}
	if err := doJSON(req, &r); err != nil {
		return "", err
	}
	if r.Metadata.AccessToken == "" {
		return "", errors.New("login failed")
	}
	token = r.Metadata.AccessToken
	if err := writeFile(filepath.Join(cacheDir, "token"), []byte(token)); err != nil {
		log.Printf("api: %v", err) // the token still works; it's just not kept across restarts
	}
	return token, nil
}

func doJSON(req *http.Request, out any) error {
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(out); err != nil {
		// Usually a Cloudflare challenge page instead of JSON.
		return fmt.Errorf("%s %s: HTTP %s: %w", req.Method, req.URL.Path, resp.Status, err)
	}
	return nil
}

// api GETs path and decodes the response's "data" into out. It re-logs in once on auth errors.
// hi requests (playback) jump the rate-limit queue.
func api(path string, out any, hi bool) error {
	for attempt := 0; ; attempt++ {
		tok, err := login(hi)
		if err != nil {
			return err
		}
		wait(hi)
		log.Printf("api: GET %s", path)
		req, err := newRequest(context.Background(), "GET", apiBase+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		var r struct {
			Status string
			Data   json.RawMessage
			Error  struct{ Message string }
		}
		if err := doJSON(req, &r); err != nil {
			return err
		}
		if r.Status == "success" {
			return json.Unmarshal(r.Data, out)
		}
		msg := r.Error.Message
		if attempt == 0 && (strings.Contains(msg, "jwt") || strings.Contains(msg, "prijavljeni")) {
			tokenMu.Lock()
			token = ""
			os.Remove(filepath.Join(cacheDir, "token"))
			tokenMu.Unlock()
			continue
		}
		return fmt.Errorf("%s: %s", path, msg)
	}
}

// cached returns fetch()'s result, memoized in memory and on disk for refreshTTL.
// If a refresh fails, the stale copy is served.
func cached[T any](key string, fetch func() (T, error)) (T, error) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	mu, _ := fetchMu.LoadOrStore(key, new(sync.Mutex))
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	memMu.Lock()
	e, ok = mem[key]
	memMu.Unlock()
	if ok && time.Since(e.at) < refreshTTL {
		return e.v.(T), nil
	}
	var v T
	file := filepath.Join(cacheDir, "json", key+".json")
	if st, err := os.Stat(file); err == nil {
		b, _ := os.ReadFile(file)
		if json.Unmarshal(b, &v) == nil {
			e, ok = memEntry{v, st.ModTime()}, true
			if time.Since(st.ModTime()) < refreshTTL {
				memMu.Lock()
				mem[key] = e
				memMu.Unlock()
				return v, nil
			}
		}
	}
	release := acquire(false)
	v, err := fetch()
	release()
	memMu.Lock()
	if err == nil && ok && entries(v)*10 < entries(e.v)*9 {
		if first, seen := shrunk[key]; !seen {
			shrunk[key] = time.Now()
			err = fmt.Errorf("%d entries, down from %d; holding the old list for up to %v", entries(v), entries(e.v), shrinkAccept)
		} else if time.Since(first) < shrinkAccept {
			err = fmt.Errorf("%d entries, down from %d since %s", entries(v), entries(e.v), first.Format(time.DateTime))
		} else {
			log.Printf("%s: accepting shrink to %d entries after %v", key, entries(v), shrinkAccept)
		}
	}
	if err == nil {
		delete(shrunk, key)
	}
	memMu.Unlock()
	if err != nil {
		if ok {
			log.Printf("refresh %s failed, serving stale: %v", key, err)
			// Keep serving the stale copy for failTTL instead of retrying on every listing.
			memMu.Lock()
			mem[key] = memEntry{e.v, time.Now().Add(failTTL - refreshTTL)}
			memMu.Unlock()
			return e.v.(T), nil
		}
		return v, err
	}
	b, _ := json.Marshal(v)
	if err := writeFile(file, b); err != nil {
		log.Printf("%s: %v", key, err) // still served from memory
	}
	memMu.Lock()
	mem[key] = memEntry{v, time.Now()}
	memMu.Unlock()
	return v, nil
}

// entries counts a cached listing's entries for the shrink guard.
func entries(v any) int {
	switch v := v.(type) {
	case []Item:
		return len(v)
	case ShowInfo:
		return len(v.Seasons)
	}
	return 0
}

// catalog lists all movies (typ 1) or shows (typ 2).
func catalog(typ int) ([]Item, error) {
	return cached(catalogKey(typ), func() ([]Item, error) {
		var all []Item
		// 300 is the API's max page size.
		for off := 0; ; off += 300 {
			if off >= maxPages*300 {
				return nil, fmt.Errorf("catalog %d: over %d pages", typ, maxPages)
			}
			var page []Item
			if err := api(fmt.Sprintf("/media?sortBy=1&genres=&type=%d&query=&limit=300&offset=%d", typ, off), &page, false); err != nil {
				return nil, err
			}
			all = append(all, page...)
			if len(page) < 300 {
				return all, nil
			}
		}
	})
}

// FirstN returns the first n items, or all if n is 0 (the -limit-* flags).
func FirstN(items []Item, n int) []Item {
	if n > 0 {
		return items[:min(n, len(items))]
	}
	return items
}

// peek returns a cached listing from memory or disk, however old, without fetching it.
func peek[T any](key string) (T, bool) {
	memMu.Lock()
	e, ok := mem[key]
	memMu.Unlock()
	if ok {
		return e.v.(T), true
	}
	var v T
	b, err := os.ReadFile(filepath.Join(cacheDir, "json", key+".json"))
	return v, err == nil && json.Unmarshal(b, &v) == nil
}

// CachedCatalog returns the catalog of typ from the cache, however old, without fetching, so listing the
// filesystem never waits on upstream. The crawler keeps it fresh. Likewise CachedShow and CachedEpisodes.
func CachedCatalog(typ int) []Item { v, _ := peek[[]Item](catalogKey(typ)); return v }

// CachedShow returns a show's seasons and plot from the cache (see CachedCatalog).
func CachedShow(showID int) ShowInfo { v, _ := peek[ShowInfo](showKey(showID)); return v }

// CachedEpisodes returns a season's episodes from the cache (see CachedCatalog).
func CachedEpisodes(showID, season int) []Item {
	v, _ := peek[[]Item](episodesKey(showID, season))
	return v
}

func catalogKey(typ int) string             { return fmt.Sprintf("catalog-%d", typ) }
func showKey(showID int) string             { return fmt.Sprintf("show-%d", showID) }
func episodesKey(showID, season int) string { return fmt.Sprintf("episodes-%d-%d", showID, season) }

func showMeta(showID int) (ShowInfo, error) {
	return cached(showKey(showID), func() (ShowInfo, error) {
		var d ShowInfo
		err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", showID), &d, false)
		return d, err
	})
}

func episodes(showID, season int) ([]Item, error) {
	return cached(episodesKey(showID, season), func() ([]Item, error) {
		var eps []Item
		err := api(fmt.Sprintf("/media/episodes/%d/%d", showID, season), &eps, false)
		return eps, err
	})
}

// readAll reads r, failing if it holds more than limit bytes. Upstream responses are bounded, not trusted to be.
func readAll(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("response over %d bytes", limit)
	}
	return b, err
}

// writeFile replaces name atomically: a concurrent reader (a listing) never sees it empty or half-written,
// and a failed write (a full disk) leaves the old content.
func writeFile(name string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // after a successful rename there's nothing left to remove
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

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
	defer acquire(false)()
	wait(false)
	log.Printf("sub: GET %s", loc)
	req, err := newRequest(context.Background(), "GET", subBase+url.PathEscape(loc), nil)
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
		return false, fmt.Errorf("subtitle %s: %s", loc, resp.Status)
	}
	b, err := readAll(resp.Body, maxSubtitle)
	if err != nil {
		return false, fmt.Errorf("subtitle %s: %w", loc, err)
	}
	old, _ := os.ReadFile(subFile(loc))
	if err := writeFile(subFile(loc), b); err != nil {
		return false, err
	}
	// After the content: a new ETag with the old content would keep it stale for good.
	return !bytes.Equal(old, b), writeFile(subFile(loc)+".etag", []byte(resp.Header.Get("ETag")))
}
