package sloflix

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/dood"
)

const (
	// Stream links expire after 7.5–8h (measured 2026-10: 5 links, direct and embed, all valid at 7h30m and
	// "error_expired" at 8h); a rejected link is re-resolved anyway.
	urlTTL = 7*time.Hour + 30*time.Minute
	// A title that used to play stays listed this long after upstream first says it's gone, so one wrong
	// "gone" (an anti-bot page, a CDN hiccup) can't make Jellyfin drop it and its watch history.
	goneGrace = 24 * time.Hour
	// Playback failures are remembered this long, so a probe retrying a dead title doesn't refetch each time.
	playFailTTL = 5 * time.Minute
	// minVideoSize is the smallest file treated as a video. Smaller ones are broken uploads (seen: 32 KB and
	// 64 KB films, a 1.6 MB episode) that ffprobe can't read, so they're hidden like titles without a source.
	minVideoSize = 5 << 20
)

// errFailedRecently is a playback lookup's failure, logged when it happened, repeated within playFailTTL.
var errFailedRecently = errors.New("lookup failed recently")

// Quiet reports whether err was already logged (errFailedRecently), so a caller doesn't log it again.
func Quiet(err error) bool { return errors.Is(err, errFailedRecently) }

var (
	lookupMu    sync.Mutex
	failed      = map[int]time.Time{} // failed playback lookups
	crawlFailed = map[int]crawlFail{} // failed crawler checks, kept apart so they don't block playback
)

// Meta is what we persist per playable item: size for getattr, subtitle for listing the .vtt.
// Size 0 means no playable source was found. Changed is when the size or subtitle last changed, as seen by
// a recheck, a stream's response (SetSize) or the crawler revalidating the subtitle; it bumps the mtime.
type Meta struct {
	Size    int64     `json:"size"`
	Sub     string    `json:"sub"`
	Plot    string    `json:"plot"`
	Changed time.Time `json:"changed"`
	Gone    time.Time `json:"gone,omitzero"` // when upstream first reported a listed title gone (see goneGrace)
	// DeadCode is a DoodStream ID reported deleted. IDs aren't reused, so while sloflix keeps returning it,
	// rechecks of a hidden title skip DoodStream.
	DeadCode string `json:"dead_code,omitempty"`
	// URL is the last stream link and URLAt when it was resolved, used for urlTTL, also after a restart.
	URL   string    `json:"url,omitempty"`
	URLAt time.Time `json:"url_at,omitzero"`
}

// sharedErr is a failure every title shares: sloflix's API or DoodStream (down, a captcha, the lookup
// limit), or the cache disk. Unlike a dead video server or a missing source, it's no reason to check a
// title less often.
type sharedErr struct{ error }

func (e sharedErr) Unwrap() error { return e.error }

// source is one of a title's upstream sources: a link to sloflix's player or a video host's embed.
type source struct {
	Source string  `json:"media_source"`
	Name   string  `json:"media_source_name"`
	Sub    *string `json:"subtitle_location"`
}

// pickSource prefers sloflix's own direct link (stream); otherwise it returns the first DoodStream embed's
// video code, to extract a link from. sub is the chosen source's subtitle.
func pickSource(sources []source) (stream, doodCode string, sub *string) {
	for _, s := range sources {
		u, err := url.Parse(s.Source)
		if err != nil {
			continue
		}
		if u.Host == "player.sloflix.com" {
			return strings.TrimSpace(u.Query().Get("source")), "", s.Sub // some carry a trailing newline
		}
		// Embed (/e/) or download-page (/d/) links; the mirror serves either code under /e/.
		if doodCode == "" && strings.Contains(strings.ToLower(s.Name), "doodstream") && (strings.HasPrefix(u.Path, "/e/") || strings.HasPrefix(u.Path, "/d/")) {
			doodCode, sub = path.Base(u.Path), s.Sub
		}
	}
	return "", doodCode, sub
}

// describeSources lists sources as name@host, for the error of a title without a usable one.
func describeSources(sources []source) []string {
	var out []string
	for _, s := range sources {
		host := s.Source // some don't parse as URLs
		if u, err := url.Parse(s.Source); err == nil {
			host = u.Host
		}
		out = append(out, fmt.Sprintf("%s@%s", s.Name, host))
	}
	return out
}

// resolve fetches a fresh stream URL for id and records it, its size and subtitle in meta.
func resolve(id int, hi bool) (string, Meta, error) {
	release := acquire(hi)
	defer func() { release() }() // not defer release(): it's replaced while waiting for DoodStream
	var d struct {
		Plot    string   `json:"media_description"`
		Sources []source `json:"media_sources"`
	}
	if err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", id), &d, hi); err != nil {
		return "", Meta{}, sharedErr{err}
	}
	m := Meta{Plot: d.Plot}
	stream, doodCode, sub := pickSource(d.Sources)
	if sub != nil {
		m.Sub = *sub
	}
	// missing is set when the source exists but its video is gone; that's persisted as no source, while
	// other errors (dead hosts, timeouts) are transient.
	prev, _, hasPrev := readMeta(id)
	var missing error
	if stream == "" && doodCode != "" {
		var err error
		if doodCode == prev.DeadCode && prev.Size == 0 { // a title in goneGrace (Size > 0) is verified again
			err = dood.ErrGone
		} else {
			stream, err = dood.URL(doodCode, func() error {
				// Give up the concurrency slot while queued for DoodStream, so lookups that don't need it go on.
				release()
				defer func() { release = acquire(hi) }()
				if hi {
					return dood.TakePlayback()
				}
				return dood.TakeBackground()
			})
		}
		if err != nil {
			if !errors.Is(err, dood.ErrGone) {
				return "", Meta{}, sharedErr{err}
			}
			missing = fmt.Errorf("DoodStream %s: %w", doodCode, err)
			m.DeadCode = doodCode
		}
	}
	if stream != "" && hi && prev.Size > 0 {
		// Playback skips the size probe: the stream checks the size on every response (stream.open). Nothing
		// was verified, so a title in its goneGrace period stays in it for the crawler to recheck.
		m.Size, m.Gone = prev.Size, prev.Gone
	} else if stream != "" {
		start := time.Now()
		size, err := cdn.ProbeSize(stream)
		if err == nil {
			u, _ := url.Parse(stream)
			log.Printf("cdn: probe %d %s %v", id, u.Host, time.Since(start).Round(time.Millisecond))
		}
		if errors.Is(err, cdn.ErrFileGone) {
			stream, missing = "", err
		} else if err != nil {
			return "", Meta{}, err
		} else if size < minVideoSize {
			stream, missing = "", fmt.Errorf("file is only %d bytes, likely a broken upload", size)
			size = 0 // no source: rechecked after refreshTTL, in case it's re-uploaded
			os.Remove(headFile(id))
		}
		m.Size = size
	}
	var noSource error
	if stream == "" && missing != nil {
		noSource = fmt.Errorf("no direct source: %w", missing)
	} else if stream == "" {
		noSource = fmt.Errorf("no direct source (got %d: %q)", len(d.Sources), describeSources(d.Sources))
	}
	m.Changed = prev.Changed
	if noSource != nil && prev.Size > 0 {
		gone := prev.Gone
		if gone.IsZero() {
			gone = time.Now()
		}
		if time.Since(gone) < goneGrace {
			m.Size, m.Gone = prev.Size, gone
			writeMeta(id, m)
			return "", m, fmt.Errorf("%w; still listed until %s", noSource, gone.Add(goneGrace).Format(time.DateTime))
		}
	}
	if hasPrev && (prev.Size != m.Size || prev.Sub != m.Sub) {
		log.Printf("media %d changed: size %d -> %d, sub %q -> %q", id, prev.Size, m.Size, prev.Sub, m.Sub)
		m.Changed = time.Now()
		if prev.Size > 0 && prev.Size != m.Size {
			os.Remove(headFile(id)) // it's from the old file
		}
	}
	if noSource != nil {
		writeMeta(id, m)
		return "", m, noSource
	}
	m.URL, m.URLAt = stream, time.Now()
	if err := writeMeta(id, m); err != nil {
		if !hi { // unrecorded, the title isn't listed: retry it later
			return "", m, sharedErr{err}
		}
		log.Printf("media %d: %v", id, err) // the link still plays
	}
	if !hi && prev.Size == 0 && probeCache > 0 { // a new title: Jellyfin will probe it (see saveHeads)
		writeFile(wantFile(id), nil)
	}
	return stream, m, nil
}

// Info returns a title's persisted meta and whether it's listed: verified by the crawler as playable. A
// playable title stays listed through transient errors and upstream reporting it gone within goneGrace
// (resolve keeps its size), so Jellyfin doesn't drop it and its watch history over a glitch.
func Info(id int) (Meta, bool) {
	m, _, _ := readMeta(id)
	return m, m.Size >= minVideoSize
}

func metaFile(id int) string {
	return filepath.Join(cacheDir, "meta", fmt.Sprint(id)+".json")
}

func writeMeta(id int, m Meta) error {
	b, _ := json.Marshal(m)
	return writeFile(metaFile(id), b)
}

func readMeta(id int) (Meta, time.Time, bool) {
	var m Meta
	st, err := os.Stat(metaFile(id))
	if err != nil {
		return m, time.Time{}, false
	}
	b, _ := os.ReadFile(metaFile(id))
	return m, st.ModTime(), json.Unmarshal(b, &m) == nil
}

// SetSize records a size seen by a stream that differs from the cached one: the upstream file was replaced.
func SetSize(id int, size int64) {
	m, _, _ := readMeta(id)
	log.Printf("media %d changed: size %d -> %d", id, m.Size, size)
	m.Size, m.Changed = size, time.Now()
	if err := writeMeta(id, m); err != nil {
		log.Printf("media %d: %v", id, err)
	}
	os.Remove(headFile(id)) // it's from the old file
}

// StreamURL returns the stream URL in meta and its size, or fresh ones if force is set or it is older than urlTTL.
// Resolving also refreshes the item's persisted meta, so opening a file revalidates its size and subtitle.
func StreamURL(id int, force bool) (string, int64, error) {
	if m, _, _ := readMeta(id); !force && m.URL != "" && time.Since(m.URLAt) < urlTTL {
		return m.URL, m.Size, nil
	}
	lookupMu.Lock()
	recent := !force && time.Since(failed[id]) < playFailTTL
	lookupMu.Unlock()
	if recent {
		return "", 0, errFailedRecently
	}
	u, m, err := resolve(id, true)
	if err != nil {
		lookupMu.Lock()
		failed[id] = time.Now()
		lookupMu.Unlock()
	}
	return u, m.Size, err
}
