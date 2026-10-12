package fusefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/cdn"
	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

// skipMax is the largest forward gap read and discarded rather than opening a new connection.
const skipMax = 1 << 20

// readTimeout bounds one read from the CDN, after which a stalled connection is reopened.
var readTimeout = 30 * time.Second

// stream serves reads from a single HTTP Range response, reopening it only on seeks.
type stream struct {
	id      int
	mu      sync.Mutex
	size    int64
	resolve func(force bool) (string, error)
	resized func(size int64) // called when a response shows the upstream file has a new size
	served  bool             // data was already returned, so a size change would splice two files
	body    io.ReadCloser
	pos     int64
	head    *os.File // the saved head for Jellyfin's probe, if any
	headLen int64

	conns  int   // connections opened, for the summary logged on close
	read   int64 // bytes downloaded, including skipped gaps
	cached int64 // bytes served from head
	opened time.Time
}

func (s *stream) close() {
	if s.body != nil {
		s.body.Close()
		s.body = nil
	}
}

func (s *stream) open(off int64) error {
	s.close()
	for attempt := range 2 {
		u, err := s.resolve(attempt > 0)
		if err != nil {
			return err
		}
		resp, err := cdn.GetRange(context.Background(), u, off)
		if err != nil {
			return err // a new link would most likely point to the same dead server
		}
		if resp.StatusCode == http.StatusPartialContent {
			if size := cdn.RangeSize(resp); size > 0 && size != s.size {
				if s.served {
					resp.Body.Close()
					return fmt.Errorf("upstream file changed from %d to %d bytes while open", s.size, size)
				}
				s.size = size
				if s.head != nil { // it's the old file's
					s.head.Close()
					s.head = nil
				}
				if s.resized != nil {
					s.resized(size)
				}
			}
			s.body, s.pos = resp.Body, off
			s.conns++
			return nil
		}
		resp.Body.Close()
		log.Printf("stream %d: %s, re-resolving", s.id, resp.Status)
	}
	return errors.New("stream unavailable")
}

func (s *stream) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.readHead(dest, off); ok {
		return fuse.ReadResultData(b), 0
	}
	buf := dest
	for range 2 {
		if off >= s.size {
			return fuse.ReadResultData(nil), 0
		}
		if s.body == nil || off < s.pos || off-s.pos > skipMax {
			if err := s.open(off); err != nil {
				log.Printf("stream %d: %v", s.id, err)
				return nil, syscall.EIO
			}
			if off >= s.size { // the first response may have shown a smaller file
				return fuse.ReadResultData(nil), 0
			}
		} else if err := s.skipTo(off); err != nil {
			log.Printf("stream %d: skip to %d: %v", s.id, off, err)
			s.close()
			continue
		}
		dest := buf[:min(int64(len(buf)), s.size-off)]
		var n int
		err := s.withDeadline(func(r io.Reader) (err error) { n, err = io.ReadFull(r, dest); return err })
		s.pos += int64(n)
		s.read += int64(n)
		if err == nil {
			s.served = true
			return fuse.ReadResultData(dest), 0
		}
		log.Printf("stream %d: read at %d: %v", s.id, off, err)
		s.close()
	}
	return nil, syscall.EIO
}

// readHead serves a read from the saved head. Reads must be whole (a short read means EOF), so only those
// entirely within it are.
func (s *stream) readHead(dest []byte, off int64) ([]byte, bool) {
	n := min(int64(len(dest)), s.size-off)
	if s.head == nil || n <= 0 || off+n > s.headLen {
		return nil, false
	}
	if got, _ := s.head.ReadAt(dest[:n], off); got != int(n) {
		return nil, false
	}
	s.served = true
	s.cached += n
	return dest[:n], true
}

// skipTo reads and discards the open body up to off.
func (s *stream) skipTo(off int64) error {
	if off == s.pos {
		return nil
	}
	err := s.withDeadline(func(r io.Reader) error { n, err := io.CopyN(io.Discard, r, off-s.pos); s.read += n; return err })
	if err == nil {
		s.pos = off
	}
	return err
}

// withDeadline runs fn on the open body, closing the body if fn takes longer than readTimeout.
func (s *stream) withDeadline(fn func(io.Reader) error) error {
	body := s.body
	t := time.AfterFunc(readTimeout, func() { body.Close() })
	defer t.Stop()
	return fn(body)
}

func (s *stream) Release(ctx context.Context) syscall.Errno {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.close()
	if s.head != nil {
		s.head.Close()
	}
	sloflix.Probed(s.id)
	if s.conns > 0 || s.cached > 0 {
		log.Printf("close %d: %d connections, %.1f MB in %v, %.1f MB from probe cache", s.id, s.conns, float64(s.read)/1e6, time.Since(s.opened).Round(100*time.Millisecond), float64(s.cached)/1e6)
	}
	return 0
}
