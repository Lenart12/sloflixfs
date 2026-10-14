// Package jellyfin asks Jellyfin to scan libraries, which it can't do by itself for a FUSE mount: FUSE has
// no change notifications.
package jellyfin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Lenart12/sloflixfs/internal/logx"
)

var (
	cooldown = time.Hour // least time between scans
	client   = &http.Client{Timeout: 30 * time.Second}
)

// Scanner scans the Jellyfin libraries under Path once new titles are added. A nil Scanner does nothing.
type Scanner struct {
	url, key, path string

	busy sync.Mutex // one scan request at a time

	mu      sync.Mutex
	pending int       // titles added since the last scan
	last    time.Time // when the last scan was started
}

// New returns a Scanner for the Jellyfin server at url, or nil if url is empty.
func New(url, key, path string) *Scanner {
	if url == "" {
		return nil
	}
	return &Scanner{url: strings.TrimSuffix(url, "/"), key: key, path: path}
}

// Add counts a newly listed title for the next scan.
func (s *Scanner) Add() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pending++
	s.mu.Unlock()
}

// Scan starts a scan of the libraries under the path. It skips it if no title was added since the last
// scan, the last scan was under cooldown ago, or Jellyfin is already scanning.
func (s *Scanner) Scan() {
	if s == nil || !s.busy.TryLock() {
		return
	}
	defer s.busy.Unlock()
	s.mu.Lock()
	n, last := s.pending, s.last
	s.mu.Unlock()
	if n == 0 || time.Since(last) < cooldown {
		return
	}
	ids, names, err := s.idleLibraries()
	if err != nil {
		logx.Warnf("jellyfin: %v", err)
		return
	}
	if ids == nil {
		return
	}
	for _, id := range ids {
		err := s.do("POST", "/Items/"+id+"/Refresh?Recursive=true&MetadataRefreshMode=Default&ImageRefreshMode=Default&ReplaceAllMetadata=false&ReplaceAllImages=false", nil)
		if err != nil {
			logx.Warnf("jellyfin: %v", err)
			return
		}
	}
	s.mu.Lock()
	s.pending -= n // titles added meanwhile wait for the next scan
	s.last = time.Now()
	s.mu.Unlock()
	logx.Infof("jellyfin: scanning %s (%d new titles)", strings.Join(names, ", "), n)
}

// idleLibraries returns the libraries under the path, or none if Jellyfin is scanning any of them or all.
func (s *Scanner) idleLibraries() (ids, names []string, err error) {
	var tasks []struct{ Key, State string }
	if err := s.do("GET", "/ScheduledTasks", &tasks); err != nil {
		return nil, nil, err
	}
	for _, t := range tasks {
		if t.Key == "RefreshLibrary" && t.State == "Running" {
			return nil, nil, nil
		}
	}
	var libs []struct {
		Name, ItemId, RefreshStatus string // RefreshStatus: Idle, Queued or Active
		Locations                   []string
	}
	if err := s.do("GET", "/Library/VirtualFolders", &libs); err != nil {
		return nil, nil, err
	}
	for _, l := range libs {
		if !slices.ContainsFunc(l.Locations, s.under) {
			continue
		}
		if l.RefreshStatus != "Idle" {
			return nil, nil, nil
		}
		ids, names = append(ids, l.ItemId), append(names, l.Name)
	}
	if ids == nil {
		return nil, nil, fmt.Errorf("no library under %s", s.path)
	}
	return ids, names, nil
}

func (s *Scanner) under(loc string) bool {
	return loc == s.path || strings.HasPrefix(loc, strings.TrimSuffix(s.path, "/")+"/")
}

func (s *Scanner) do(method, path string, out any) error {
	req, err := http.NewRequest(method, s.url+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+s.key+`"`)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, req.URL.Path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
