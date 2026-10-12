package sloflix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/web"
)

// apiBase is sloflix's API.
var apiBase = "https://api.sloflix.com/v1"

// startTime stands in for titles without a valid creation time.
var startTime = time.Now()

const (
	// A failed login (bad credentials, sloflix down) is retried after this.
	loginRetry = 5 * time.Minute
	// maxJSON bounds an API response, far above a real catalog page (~0.2 MB).
	maxJSON = 64 << 20
)

var (
	apiClient = &http.Client{Timeout: 30 * time.Second}

	tokenMu  sync.Mutex
	token    string
	loginErr error     // the last login's error
	loginAt  time.Time // when it failed
)

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

// login returns the API token from memory, disk or a new login. A failed login is remembered for
// loginRetry, so bad credentials don't cost a request per API call.
func login(p priority) (_ string, err error) {
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
	wait(p)
	log.Printf("api: login")
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, err := web.NewRequest(context.Background(), "POST", apiBase+"/user/login", bytes.NewReader(body))
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
		log.Printf("api: saving token: %v", err) // the token still works; it's just not kept across restarts
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
// Playback requests jump the rate-limit queue.
func api(path string, out any, p priority) error {
	for attempt := 0; ; attempt++ {
		tok, err := login(p)
		if err != nil {
			return err
		}
		wait(p)
		log.Printf("api: GET %s", path)
		req, err := web.NewRequest(context.Background(), "GET", apiBase+path, nil)
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
		return fmt.Errorf("%s: %q", path, msg)
	}
}
