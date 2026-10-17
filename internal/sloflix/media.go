package sloflix

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/dood"
	"github.com/Lenart12/sloflixfs/internal/logx"
	"github.com/Lenart12/sloflixfs/internal/mp4"
)

const (
	// Stream links expire between 7h30 and 8h. A rejected link is re-resolved anyway.
	urlTTL = 7*time.Hour + 30*time.Minute
	// A title that used to play stays listed this long after upstream first says it's gone, so a glitch
	// can't make Jellyfin drop it and its watch history.
	goneGrace = 24 * time.Hour
	// Playback failures are remembered this long, so retries of a dead title don't refetch it.
	playFailTTL = 5 * time.Minute
	// minVideoSize is the smallest file treated as a video. Smaller ones are broken uploads, hidden like
	// titles without a source.
	minVideoSize = 5 << 20
)

// errFailedRecently repeats a playback lookup's failure within playFailTTL. It was logged when it happened.
var errFailedRecently = errors.New("lookup failed recently")

// Quiet reports whether err was already logged.
func Quiet(err error) bool { return errors.Is(err, errFailedRecently) }

var (
	lookupMu    sync.Mutex
	failed      = map[int]time.Time{} // failed playback lookups
	crawlFailed = map[int]crawlFail{} // failed crawler checks, kept apart so they don't block playback
)

// Meta is what's persisted per title. Size 0 means no playable source was found. Changed is when the size
// or subtitle last changed, which bumps the files' mtime.
type Meta struct {
	Size    int64     `json:"size"`
	Sub     string    `json:"sub"`
	Plot    string    `json:"plot"`
	Changed time.Time `json:"changed"`
	Gone    time.Time `json:"gone,omitzero"` // when upstream first reported a listed title gone
	// DeadCode is a DoodStream code reported deleted. Codes aren't reused, so rechecks skip DoodStream while
	// sloflix still returns it.
	DeadCode string `json:"dead_code,omitempty"`
	// URL is the last stream link and URLAt when it was resolved.
	URL   string    `json:"url,omitempty"`
	URLAt time.Time `json:"url_at,omitzero"`
}

// sharedErr is a failure all titles share, of sloflix's API, DoodStream or the cache disk. It's no reason
// to check a title less often.
type sharedErr struct{ error }

func (e sharedErr) Unwrap() error { return e.error }

// source is one of a title's upstream sources: a link to sloflix's player or a video host's embed.
type source struct {
	Source string  `json:"media_source"`
	Name   string  `json:"media_source_name"`
	Sub    *string `json:"subtitle_location"`
}

// pickSource returns sloflix's direct link if there is one, otherwise the first DoodStream video code, and
// the chosen source's subtitle.
func pickSource(sources []source) (stream, doodCode string, sub *string) {
	for _, s := range sources {
		u, err := url.Parse(s.Source)
		if err != nil {
			continue
		}
		if u.Host == "player.sloflix.com" {
			return strings.TrimSpace(u.Query().Get("source")), "", s.Sub // some carry a trailing newline
		}
		// Embed (/e/) or download (/d/) links: the mirror serves either code under /e/.
		if doodCode == "" && strings.Contains(strings.ToLower(s.Name), "doodstream") && (strings.HasPrefix(u.Path, "/e/") || strings.HasPrefix(u.Path, "/d/")) {
			doodCode, sub = path.Base(u.Path), s.Sub
		}
	}
	return "", doodCode, sub
}

// describeSources lists sources as name@host.
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
func resolve(id int, p priority) (string, Meta, error) {
	release := acquire(p)
	defer func() { release() }() // not defer release(): it's replaced while waiting for DoodStream
	d, err := details(id, p)
	if err != nil {
		return "", Meta{}, sharedErr{err}
	}
	m := Meta{Plot: d.Plot}
	stream, code, sub := pickSource(d.Sources)
	if sub != nil {
		m.Sub = *sub
	}
	prev, checked, hasPrev := readMeta(id)
	// missing means the video is gone, which is recorded. Other errors are transient.
	var missing error
	if stream == "" && code != "" {
		stream, missing, err = doodStream(code, prev, func() error {
			// Free the concurrency slot while queued for DoodStream.
			release()
			defer func() { release = acquire(p) }()
			return takeDood(p)
		})
		if errors.Is(err, dood.ErrEncoding) { // this video's own problem, so it backs off
			return "", Meta{}, err
		}
		if err != nil {
			return "", Meta{}, sharedErr{err}
		}
		if missing != nil {
			m.DeadCode = code
		}
	}
	unprobed := stream != "" && p == playback && prev.Size > 0
	if unprobed {
		// Playback skips the probe: the stream checks the size on every response. Nothing was verified, so
		// a title in goneGrace stays in it.
		m.Size, m.Gone = prev.Size, prev.Gone
	} else if stream != "" {
		if m.Size, missing, err = measure(id, stream); err != nil {
			return "", Meta{}, err
		}
		if missing != nil {
			stream = ""
		}
	}
	m.Changed = prev.Changed
	if stream == "" {
		m, err := recordNoSource(id, prev, hasPrev, m, noSource(missing, d.Sources))
		return "", m, err
	}
	if hasPrev {
		noteChange(id, prev, &m)
	}
	m.URL, m.URLAt = stream, time.Now()
	if err := recordPlayable(id, p, prev.Size == 0, m); err != nil {
		return "", m, err
	}
	if unprobed {
		// Keep the meta's mtime, the last check: Jellyfin reopens a file it can't read on every scan, which
		// would otherwise keep a broken title from its recheck forever.
		os.Chtimes(metaFile(id), checked, checked)
	}
	return stream, m, nil
}

// titleDetails is the part of a title's API details that resolve uses.
type titleDetails struct {
	Plot    string   `json:"media_description"`
	Sources []source `json:"media_sources"`
}

func details(id int, p priority) (titleDetails, error) {
	var d titleDetails
	err := api(fmt.Sprintf("/media/single/%d?dont_count_view=true", id), &d, p)
	return d, err
}

// noSource is the error for a title without a playable source.
func noSource(missing error, sources []source) error {
	if missing != nil {
		return fmt.Errorf("no direct source: %w", missing)
	}
	return fmt.Errorf("no direct source (got %d: %q)", len(sources), describeSources(sources))
}

// doodStream gets a stream link for a DoodStream video code. missing is set if the video was deleted.
func doodStream(code string, prev Meta, take func() error) (stream string, missing, err error) {
	if code == prev.DeadCode && prev.Size == 0 { // titles in goneGrace are verified again
		return "", fmt.Errorf("DoodStream %s: %w", code, dood.ErrGone), nil
	}
	stream, err = dood.URL(code, take)
	if errors.Is(err, dood.ErrGone) {
		return "", fmt.Errorf("DoodStream %s: %w", code, err), nil
	}
	return stream, nil, err
}

func takeDood(p priority) error {
	if p == playback {
		return dood.TakePlayback()
	}
	return dood.TakeBackground()
}

// measure probes a stream's size. missing is set if the file is gone or isn't a readable video.
func measure(id int, stream string) (size int64, missing, err error) {
	start := time.Now()
	size, head, err := cdn.Probe(stream)
	if errors.Is(err, cdn.ErrFileGone) {
		return 0, err, nil
	}
	if err != nil {
		return 0, nil, err
	}
	u, _ := url.Parse(stream)
	logx.Debugf("cdn: probe %d %s %v", id, u.Host, time.Since(start).Round(time.Millisecond))
	if size < minVideoSize {
		os.Remove(headFile(id))
		return 0, fmt.Errorf("file is only %d bytes, likely a broken upload", size), nil
	}
	if err := mp4.Check(head); err != nil {
		os.Remove(headFile(id))
		return 0, fmt.Errorf("not a readable video, likely a broken upload: %v", err), nil
	}
	return size, nil, nil
}

// recordNoSource records that id has no playable source. A title that used to play stays listed for
// goneGrace after upstream first reports it gone.
func recordNoSource(id int, prev Meta, hasPrev bool, m Meta, reason error) (Meta, error) {
	if prev.Size > 0 {
		gone := prev.Gone
		if gone.IsZero() {
			gone = time.Now()
		}
		if time.Since(gone) < goneGrace {
			m.Size, m.Gone = prev.Size, gone
			writeMeta(id, m)
			return m, fmt.Errorf("%w; still listed until %s", reason, gone.Add(goneGrace).Format(time.DateTime))
		}
	}
	if hasPrev {
		noteChange(id, prev, &m)
	}
	writeMeta(id, m)
	return m, reason
}

// noteChange bumps m.Changed if its size or subtitle differ from prev, and drops the old file's head.
func noteChange(id int, prev Meta, m *Meta) {
	if prev.Size == m.Size && prev.Sub == m.Sub {
		return
	}
	logx.Infof("media %d changed: size %d -> %d, sub %q -> %q", id, prev.Size, m.Size, prev.Sub, m.Sub)
	m.Changed = time.Now()
	if prev.Size > 0 && prev.Size != m.Size {
		os.Remove(headFile(id))
	}
}

// recordPlayable records a verified title, and marks a new one for saveHeads.
func recordPlayable(id int, p priority, isNew bool, m Meta) error {
	if err := writeMeta(id, m); err != nil {
		if p == background { // not listed: the crawler retries it
			return sharedErr{err}
		}
		logx.Warnf("media %d: saving meta: %v", id, err) // the link still plays
	}
	if p == background && isNew && probeCache > 0 {
		writeFile(wantFile(id), nil)
	}
	return nil
}

// Info returns a title's persisted meta and whether it's listed, which it is once the crawler verified it
// as playable.
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

// SetSize records a new size a stream saw: the upstream file was replaced.
func SetSize(id int, size int64) {
	m, _, _ := readMeta(id)
	logx.Infof("media %d changed: size %d -> %d", id, m.Size, size)
	m.Size, m.Changed = size, time.Now()
	if err := writeMeta(id, m); err != nil {
		logx.Warnf("media %d: saving meta: %v", id, err)
	}
	os.Remove(headFile(id))
}

// StreamURL returns title id's stream URL and size, resolving a new URL if force is set or the saved one
// is older than urlTTL.
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
	u, m, err := resolve(id, playback)
	if err != nil {
		lookupMu.Lock()
		failed[id] = time.Now()
		lookupMu.Unlock()
	}
	return u, m.Size, err
}
