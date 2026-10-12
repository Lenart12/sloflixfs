// Package jellyfin asks Jellyfin to scan libraries, which it can't do by itself for a FUSE mount: FUSE has
// no change notifications.
package jellyfin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
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

// Scan starts a scan of the libraries under the path, unless no title was added since the last one, that
// was under cooldown ago, or Jellyfin is already scanning.
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
	var tasks []struct{ Key, State string }
	if err := s.do("GET", "/ScheduledTasks", &tasks); err != nil {
		log.Printf("jellyfin: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Key == "RefreshLibrary" && t.State == "Running" {
			return
		}
	}
	var libs []struct {
		Name, ItemId, RefreshStatus string // RefreshStatus: Idle, Queued or Active
		Locations                   []string
	}
	if err := s.do("GET", "/Library/VirtualFolders", &libs); err != nil {
		log.Printf("jellyfin: %v", err)
		return
	}
	var ids, names []string
	for _, l := range libs {
		for _, loc := range l.Locations {
			if loc == s.path || strings.HasPrefix(loc, strings.TrimSuffix(s.path, "/")+"/") {
				if l.RefreshStatus != "Idle" {
					return // already scanning
				}
				ids, names = append(ids, l.ItemId), append(names, l.Name)
				break
			}
		}
	}
	if len(ids) == 0 {
		log.Printf("jellyfin: no library under %s", s.path)
		return
	}
	for _, id := range ids {
		err := s.do("POST", "/Items/"+id+"/Refresh?Recursive=true&MetadataRefreshMode=Default&ImageRefreshMode=Default&ReplaceAllMetadata=false&ReplaceAllImages=false", nil)
		if err != nil {
			log.Printf("jellyfin: %v", err)
			return
		}
	}
	s.mu.Lock()
	s.pending -= n // titles added meanwhile wait for the next scan
	s.last = time.Now()
	s.mu.Unlock()
	log.Printf("jellyfin: scanning %s (%d new titles)", strings.Join(names, ", "), n)
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
