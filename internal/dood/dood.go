// Package dood turns DoodStream video codes into direct links, within DoodStream's lookup limit.
package dood

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/web"
)

// maxPage bounds a player page read, far above the real ~0.1 MB.
const maxPage = 4 << 20

// Mirror is where embeds are opened. Sloflix's embed links often point at dead mirror domains, but video
// codes work on any mirror, and doodstream.com redirects to a live one.
var Mirror = "https://doodstream.com/e/"

// ErrGone means DoodStream reports the video deleted.
var ErrGone = errors.New("video not found on DoodStream")

var (
	passMD5   = regexp.MustCompile(`/pass_md5/[^'"]+`)
	htmlTitle = regexp.MustCompile(`<title>([^<]*)`)
)

// URL turns a DoodStream video code into a direct, tokenized MP4 URL, the same way the embed player does:
// the embed page names a /pass_md5/ path whose response is the file's base URL. take waits for a lookup
// slot.
func URL(code string, take func() error) (string, error) {
	pass, embed, err := player(code)
	if err != nil {
		return "", err
	}
	// Only the pass_md5 request counts toward DoodStream's limit, so the slot is taken only now.
	waitStart := time.Now()
	if err := take(); err != nil {
		return "", err
	}
	if time.Since(waitStart) > time.Second { // the page's pass_md5 path may have gone stale while waiting
		if pass, embed, err = player(code); err != nil {
			return "", err
		}
	}
	base, _, err := get(embed.Scheme+"://"+embed.Host+string(pass), embed.String())
	if err != nil {
		return "", err
	}
	if !bytes.HasPrefix(base, []byte("https://")) {
		return "", fmt.Errorf("doodstream %s: unexpected pass_md5 response %.40q", code, base)
	}
	return link(string(base), string(pass)), nil
}

// player opens the player page and returns its pass_md5 path and its URL after redirects.
func player(code string) ([]byte, *url.URL, error) {
	log.Printf("dood: GET %s", code)
	page, resp, err := get(Mirror+code, "https://www.sloflix.com/")
	if err != nil {
		return nil, nil, err
	}
	if p := passMD5.Find(page); p != nil {
		return p, resp.Request.URL, nil
	}
	// Only an explicit "Video not found" means deleted. Anti-bot and rate-limit pages are transient.
	var title string
	if m := htmlTitle.FindSubmatch(page); m != nil {
		title = string(m[1])
	}
	if strings.Contains(strings.ToLower(title), "video not found") {
		return nil, nil, ErrGone
	}
	if bytes.Contains(page, []byte("turnstile")) {
		return nil, nil, fmt.Errorf("doodstream %s: asks for a captcha (Turnstile)", code)
	}
	return nil, nil, fmt.Errorf("doodstream %s: no pass_md5 in page (title %q)", code, title)
}

func get(u, ref string) ([]byte, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := web.NewRequest(ctx, "GET", u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Referer", ref)
	resp, err := cdn.Client.Do(req)
	if err != nil {
		return nil, nil, cdn.Redact(err)
	}
	defer resp.Body.Close()
	b, err := web.ReadAll(resp.Body, maxPage)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("doodstream %s: %s", u, resp.Status)
	}
	return b, resp, err
}

// link builds the stream URL from pass_md5's base URL as the player's script does: 10 random characters,
// then the pass path's last element as the token.
func link(base, pass string) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	random := make([]byte, 10)
	for i := range random {
		random[i] = letters[rand.IntN(len(letters))]
	}
	return fmt.Sprintf("%s%s?token=%s&expiry=%d", base, random, path.Base(pass), time.Now().UnixMilli())
}
