package fusefs

import (
	"context"
	"log"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

type video struct {
	fs.Inode
	id    int
	mtime time.Time
}

func (v *video) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	m, ok := sloflix.Info(v.id)
	if !ok {
		return syscall.EIO
	}
	mtime := mtimeOf(v.mtime, m)
	out.Mode = fuse.S_IFREG | 0444
	out.Size = uint64(m.Size)
	out.SetTimes(nil, &mtime, &mtime)
	return 0
}

func (v *video) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	log.Printf("open %d: %s", v.id, v.Path(nil))
	s := &stream{
		id:     v.id,
		opened: time.Now(),
		resolve: func(force bool) (string, error) {
			u, _, err := sloflix.StreamURL(v.id, force)
			return u, err
		},
		resized: func(size int64) {
			sloflix.SetSize(v.id, size)
			// Drop the kernel's cached size, which clamps reads. Async, since invalidating from inside a read
			// on this inode could deadlock.
			go v.NotifyContent(-1, 0)
		},
	}
	// Jellyfin's probe of a new title is served from its saved head, without resolving a link.
	if m, ok := sloflix.Info(v.id); ok {
		if f, n, ok := sloflix.OpenHead(v.id); ok {
			s.size, s.head, s.headLen = m.Size, f, n
			return s, 0, 0
		}
	}
	_, size, err := sloflix.StreamURL(v.id, false)
	if err != nil {
		if !sloflix.Quiet(err) {
			log.Printf("open %d: %v", v.id, err)
		}
		return nil, 0, syscall.EIO
	}
	s.size = size
	return s, 0, 0
}

// mtimeOf bumps an item's mtime when its source or subtitle changed, so Jellyfin re-probes it.
func mtimeOf(created time.Time, m sloflix.Meta) time.Time {
	if m.Changed.After(created) {
		return m.Changed
	}
	return created
}
