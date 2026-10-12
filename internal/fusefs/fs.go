// Package fusefs serves the sloflix catalog as a read-only FUSE filesystem in Jellyfin's layout, with
// videos streamed from the CDN on demand.
package fusefs

import (
	"context"
	"log"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type child struct {
	name string
	dir  bool
	node func() (fs.InodeEmbedder, error)
	// visible, if set, decides whether the child is listed. It's checked lazily, so a lookup doesn't read
	// every title's meta.
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

// memFile is a read-only file served from memory.
func memFile(b []byte, mtime time.Time) *fs.MemRegularFile {
	f := &fs.MemRegularFile{Data: b}
	f.Attr.Mode = 0444
	f.Attr.SetTimes(nil, &mtime, &mtime)
	return f
}

func dirChild(name string, mtime time.Time, list func() ([]child, error)) child {
	return child{name: name, dir: true, node: func() (fs.InodeEmbedder, error) { return &dir{mtime: mtime, list: list}, nil }}
}
