package fusefs

import (
	"fmt"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"

	"github.com/Lenart12/sloflixfs/internal/sloflix"
)

var startTime = time.Now()

// NewRoot returns the filesystem root: Movies and Shows, listing only titles verified as playable.
func NewRoot() *dir {
	return &dir{mtime: startTime, list: func() ([]child, error) {
		return []child{
			dirChild("Movies", startTime, catalogDir(sloflix.Movies, movieVisible, movieDir)),
			dirChild("Shows", startTime, catalogDir(sloflix.Shows, showVisible, showDir)),
		}, nil
	}}
}

// catalogDir lists the catalog of kind, newest first. Only titles that pass visible are listed; they're
// checked lazily (see child.visible).
func catalogDir(kind sloflix.Kind, visible func(id int) bool, itemDir func(it sloflix.Item, name string) func() ([]child, error)) func() ([]child, error) {
	return func() ([]child, error) {
		items := sloflix.CachedCatalog(kind)
		names := uniqNames(items, title, visible)
		var out []child
		for _, it := range items {
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

func subtitleNode(loc string, mtime time.Time) func() (fs.InodeEmbedder, error) {
	return func() (fs.InodeEmbedder, error) {
		b, err := sloflix.Subtitle(loc)
		if err != nil {
			return nil, err
		}
		return memFile(b, mtime), nil
	}
}
