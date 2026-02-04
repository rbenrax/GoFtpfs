package ftpfs

import (
	"context"
	"log"
	"os"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

// SimpleFs is a minimal filesystem used for testing/mount sanity checks.
type SimpleFs struct {
	debug bool
}

// NewSimpleFs creates a minimal filesystem that responds immediately.
func NewSimpleFs(debug bool) *SimpleFs {
	if debug {
		log.Printf("Created SimpleFs (no FTP) for quick mount checks")
	}
	return &SimpleFs{debug: debug}
}

// Root returns the root node
func (s *SimpleFs) Root() (fs.Node, error) {
	return &SimpleNode{fs: s}, nil
}

type SimpleNode struct {
	fs *SimpleFs
}

func (n *SimpleNode) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Inode = 1
	a.Mode = os.ModeDir | 0755
	a.Atime = time.Now()
	a.Mtime = time.Now()
	return nil
}

func (n *SimpleNode) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	if n.fs.debug {
		log.Printf("SimpleFs: ReadDirAll returning placeholder entry")
	}
	entries := []fuse.Dirent{
		{Inode: 2, Name: "ftp-unavailable", Type: fuse.DT_File},
	}
	return entries, nil
}

var _ = fs.Node(&SimpleNode{})
var _ = fs.HandleReadDirAller(&SimpleNode{})
