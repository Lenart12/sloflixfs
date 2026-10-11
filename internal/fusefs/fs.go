// Package fusefs serves the sloflix catalog as a read-only FUSE filesystem in Jellyfin's layout: verified
// titles only, with NFO and subtitle sidecars, and video files streamed from the CDN on demand.
package fusefs

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

var startTime = time.Now()

type child struct {
	name string
	dir  bool
	node func() (fs.InodeEmbedder, error)
	// visible, if set, decides whether the child is listed. Readdir checks every child, Lookup only the one
	// it found, so looking up one title doesn't read every title's meta.
	visible func() bool
}

func (c child) listed() bool { return c.visible == nil || c.visible() }

// dir is a read-only directory whose entries are produced on demand by list.
type dir struct {
	fs.Inode
	mtime time.Time
	list  func() ([]child, error)
}

func (d *dir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0555
	out.SetTimes(nil, &d.mtime, &d.mtime)
	return 0
}

func (d *dir) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	children, err := d.list()
	if err != nil {
		log.Printf("readdir: %v", err)
		return nil, syscall.EIO
	}
	var entries []fuse.DirEntry
	for _, c := range children {
		if !c.listed() {
			continue
		}
		e := fuse.DirEntry{Name: c.name, Mode: fuse.S_IFREG}
		if c.dir {
			e.Mode = fuse.S_IFDIR
		}
		entries = append(entries, e)
	}
	return fs.NewListDirStream(entries), 0
}

func (d *dir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	children, err := d.list()
	if err != nil {
		log.Printf("lookup %s: %v", name, err)
		return nil, syscall.EIO
	}
	for _, c := range children {
		if c.name != name {
			continue
		}
		if !c.listed() {
			return nil, syscall.ENOENT
		}
		n, err := c.node()
		if err != nil {
			log.Printf("lookup %s: %v", name, err)
			return nil, syscall.EIO
		}
		var a fuse.AttrOut
		if errno := n.(fs.NodeGetattrer).Getattr(ctx, nil, &a); errno != 0 {
			return nil, errno
		}
		out.Attr = a.Attr
		out.SetAttrTimeout(a.Timeout())
		return d.NewInode(ctx, n, fs.StableAttr{Mode: a.Mode & syscall.S_IFMT}), 0
	}
	return nil, syscall.ENOENT
}

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
			// Drop the kernel's cached attrs so it stops clamping reads to the old size. Async: invalidating
			// from inside the read that's still in flight on this inode could deadlock.
			go v.NotifyContent(-1, 0)
		},
	}
	// A new title's start, saved by the crawler: Jellyfin's probe is served from it without resolving a link.
	if m, ok := sloflix.Info(v.id); ok {
		if f, err := os.Open(sloflix.HeadFile(v.id)); err == nil {
			if st, err := f.Stat(); err == nil {
				s.size, s.head, s.headLen = m.Size, f, st.Size()
				return s, 0, 0
			}
			f.Close()
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

func subtitleNode(loc string, mtime time.Time) func() (fs.InodeEmbedder, error) {
	return func() (fs.InodeEmbedder, error) {
		b, err := sloflix.Subtitle(loc)
		if err != nil {
			return nil, err
		}
		f := &fs.MemRegularFile{Data: b}
		f.Attr.Mode = 0444
		f.Attr.SetTimes(nil, &mtime, &mtime)
		return f, nil
	}
}

// name is the item's folder/file name: English if available (it matches TMDB best).
func name(it sloflix.Item) string {
	if it.NameEn != "" {
		return clean(it.NameEn)
	}
	return clean(it.Name)
}

// clean removes characters unsafe in file names. Some upstream titles carry invisible zero-width characters.
func clean(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '/' {
			return '-'
		}
		if !unicode.IsGraphic(r) {
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

// title is the Jellyfin folder name: "Name (Year)".
func title(it sloflix.Item) string {
	if it.Year > 0 {
		return fmt.Sprintf("%s (%d)", name(it), it.Year)
	}
	return name(it)
}

// uniqNames names each item, appending " [id]" to all but one of the items sharing a name. The plain name
// goes to the oldest (lowest ID) playable one, so a newly added duplicate gets the suffix while a re-upload
// of a dead title takes over its path; either way the path Jellyfin keeps watch history under stays.
// playable is only checked for items sharing a name.
func uniqNames(items []sloflix.Item, name func(sloflix.Item) string, playable func(id int) bool) map[int]string {
	groups := map[string][]sloflix.Item{}
	for _, it := range items {
		groups[name(it)] = append(groups[name(it)], it)
	}
	out := map[int]string{}
	for n, g := range groups {
		slices.SortFunc(g, func(a, b sloflix.Item) int {
			if pa, pb := playable(a.ID), playable(b.ID); pa != pb {
				if pa {
					return -1
				}
				return 1
			}
			return a.ID - b.ID
		})
		for i, it := range g {
			out[it.ID] = n
			if i > 0 {
				out[it.ID] = fmt.Sprintf("%s [%d]", n, it.ID)
			}
		}
	}
	return out
}

// mediaFiles lists the video file and, if present, its subtitle for item id under base name, and returns
// its meta. Items the crawler hasn't verified as playable are left out.
func mediaFiles(id int, base string, mtime time.Time) ([]child, sloflix.Meta) {
	m, ok := sloflix.Info(id)
	if !ok {
		return nil, m
	}
	out := []child{{name: base + ".mp4", node: func() (fs.InodeEmbedder, error) { return &video{id: id, mtime: mtime}, nil }}}
	if m.Sub != "" {
		out = append(out, child{name: base + ".sl.vtt", node: subtitleNode(m.Sub, mtimeOf(mtime, m))})
	}
	return out, m
}

func dirChild(name string, mtime time.Time, list func() ([]child, error)) child {
	return child{name: name, dir: true, node: func() (fs.InodeEmbedder, error) { return &dir{mtime: mtime, list: list}, nil }}
}

// catalogDir lists the catalog of typ, newest first, capped at limit entries if limit > 0. Only titles that
// pass visible are listed; they're checked lazily (see child.visible).
func catalogDir(typ, limit int, visible func(id int) bool, itemDir func(it sloflix.Item, name string) func() ([]child, error)) func() ([]child, error) {
	return func() ([]child, error) {
		items := sloflix.CachedCatalog(typ)
		names := uniqNames(items, title, visible)
		var out []child
		for _, it := range sloflix.FirstN(items, limit) {
			name := names[it.ID]
			c := dirChild(name, it.CreatedAt(), itemDir(it, name))
			c.visible = func() bool { return visible(it.ID) }
			out = append(out, c)
		}
		return out, nil
	}
}

func movieVisible(id int) bool {
	_, ok := sloflix.Info(id)
	return ok
}

// showVisible reports whether any of a show's episodes is playable. It reads cached listings only, so
// listing Shows never waits on upstream, even right after a restart before the crawler's first pass.
func showVisible(id int) bool {
	si := sloflix.CachedShow(id)
	for _, s := range si.Seasons {
		eps := sloflix.CachedEpisodes(id, s)
		for _, ep := range eps {
			if _, ok := sloflix.Info(ep.ID); ok {
				return true
			}
		}
	}
	return false
}

func movieDir(it sloflix.Item, name string) func() ([]child, error) {
	created := it.CreatedAt()
	return func() ([]child, error) {
		files, m := mediaFiles(it.ID, name, created)
		if files == nil {
			return nil, nil
		}
		return append(files, nfoChild("movie.nfo", "movie", it, m.Plot, mtimeOf(created, m))...), nil
	}
}

func showDir(show sloflix.Item, name string) func() ([]child, error) {
	mtime := show.CreatedAt()
	return func() ([]child, error) {
		si := sloflix.CachedShow(show.ID)
		out := nfoChild("tvshow.nfo", "tvshow", show, si.Plot, mtime)
		for _, s := range si.Seasons {
			list := seasonDir(show.ID, s, name)
			if files, _ := list(); len(files) == 0 {
				continue // no playable episodes yet
			}
			out = append(out, dirChild(fmt.Sprintf("Season %02d", s), mtime, list))
		}
		return out, nil
	}
}

func seasonDir(showID, season int, showName string) func() ([]child, error) {
	return func() ([]child, error) {
		eps := sloflix.CachedEpisodes(showID, season)
		names := uniqNames(eps, func(ep sloflix.Item) string { return fmt.Sprintf("%s S%02dE%02d", showName, season, ep.Episode) }, movieVisible)
		var out []child
		for _, ep := range eps {
			files, _ := mediaFiles(ep.ID, names[ep.ID], ep.CreatedAt())
			out = append(out, files...)
		}
		return out, nil
	}
}

func NewRoot(limitMovies, limitShows int) *dir {
	return &dir{mtime: startTime, list: func() ([]child, error) {
		return []child{
			dirChild("Movies", startTime, catalogDir(1, limitMovies, movieVisible, movieDir)),
			dirChild("Shows", startTime, catalogDir(2, limitShows, showVisible, showDir)),
		}, nil
	}}
}
