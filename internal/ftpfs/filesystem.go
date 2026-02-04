package ftpfs

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
)

const (
	// Root inode number
	rootInode uint64 = 1

	// TTL for FUSE attributes (30 seconds)
	attrTTL = 30 * time.Second

	// Directory cache TTL (60 seconds)
	dirCacheTTL = 60 * time.Second

	// Attribute cache TTL (120 seconds)
	attrCacheTTL = 120 * time.Second
)

// Temporary file patterns to ignore (VS Code optimization)
var tempFilePatterns = []string{
	".attach_pid",          // Java debugger
	".swp", ".swo", ".swn", // vim swap files
	"~",             // Backup files
	".tmp", ".temp", // Temporary files
	".git", ".svn", ".hg", // Version control
	".vscode", ".idea", // IDE configs
	"__pycache__", ".pyc", ".pyo", // Python cache
	".DS_Store", ".directory", // System files
	".nfs", ".lock", ".pid", // Lock files
}

// isTempFile checks if a filename is a temporary file
func isTempFile(name string) bool {
	if strings.HasPrefix(name, ".") {
		if strings.HasPrefix(name, ".attach_pid") {
			return true
		}
		if strings.HasSuffix(name, "~") {
			return true
		}
		for _, pattern := range tempFilePatterns {
			if strings.Contains(name, pattern) {
				return true
			}
		}
	}
	if strings.HasSuffix(name, "~") {
		return true
	}
	return false
}

// Inode represents a file or directory inode
type Inode struct {
	Ino     uint64
	Parent  uint64
	Name    string
	Attr    fuse.Attr
	FtpPath string
	IsDir   bool
}

// DirCacheEntry represents a cached directory listing
type DirCacheEntry struct {
	Files     []FtpFileInfo
	Timestamp time.Time
}

// AttrCacheEntry represents cached attributes
type AttrCacheEntry struct {
	Attr      fuse.Attr
	Timestamp time.Time
}

// WriteBuffer represents a write buffer for lazy writes
type WriteBuffer struct {
	Data         []byte
	Dirty        bool
	LastModified time.Time
}

// FileHandle represents an open file handle
type FileHandle struct {
	Ino         uint64
	WriteBuffer *WriteBuffer
}

// fetchInProgress tracks ongoing directory fetches
type fetchInProgress struct {
	ch    chan struct{}
	files []FtpFileInfo
	err   error
}

// FtpFs represents the FTP filesystem
type FtpFs struct {
	ftpConn *SimpleFTPConnection
	debug   bool

	// Inode management
	inodes      map[uint64]*Inode
	pathToInode map[string]uint64
	nextInode   uint64
	inodeMu     sync.RWMutex

	// Caches
	dirCache  map[string]*DirCacheEntry
	attrCache map[uint64]*AttrCacheEntry
	readCache map[uint64][]byte
	cacheMu   sync.RWMutex

	// Fetch coordination - prevents duplicate simultaneous fetches
	fetchMap map[string]*fetchInProgress
	fetchMu  sync.Mutex

	// Open file handles
	openFiles map[uint64]*FileHandle
	nextFH    uint64
	handleMu  sync.RWMutex

	// Default UID/GID
	uid uint32
	gid uint32
}

// NewFtpFs creates a new FTP filesystem
func NewFtpFs(ftpConn *SimpleFTPConnection, debug bool) *FtpFs {
	fs := &FtpFs{
		ftpConn:     ftpConn,
		debug:       debug,
		inodes:      make(map[uint64]*Inode),
		pathToInode: make(map[string]uint64),
		nextInode:   2, // Start at 2, 1 is reserved for root
		dirCache:    make(map[string]*DirCacheEntry),
		attrCache:   make(map[uint64]*AttrCacheEntry),
		readCache:   make(map[uint64][]byte),
		fetchMap:    make(map[string]*fetchInProgress),
		openFiles:   make(map[uint64]*FileHandle),
		nextFH:      1,
		uid:         uint32(os.Getuid()),
		gid:         uint32(os.Getgid()),
	}

	// Create root inode
	now := time.Now()
	rootAttr := fuse.Attr{
		Inode: rootInode,
		Size:  0,
		Mode:  os.ModeDir | 0755,
		Uid:   fs.uid,
		Gid:   fs.gid,
		Atime: now,
		Mtime: now,
		Ctime: now,
	}

	root := &Inode{
		Ino:     rootInode,
		Parent:  rootInode,
		Name:    "/",
		Attr:    rootAttr,
		FtpPath: "/",
		IsDir:   true,
	}

	fs.inodes[rootInode] = root
	fs.pathToInode["/"] = rootInode
	fs.attrCache[rootInode] = &AttrCacheEntry{
		Attr:      rootAttr,
		Timestamp: time.Now(),
	}

	if fs.debug {
		log.Printf("Created FtpFs with caching enabled")
	}
	return fs
}

// PrefetchDir synchronously loads a directory into cache (blocking until done).
func (f *FtpFs) PrefetchDir(ftpPath string) error {
	if f == nil {
		return fmt.Errorf("nil filesystem")
	}
	if f.debug {
		log.Printf("Prefetch: fetching directory %s (blocking)", ftpPath)
	}
	files, err := f.ftpConn.ListDir(ftpPath)
	if err != nil {
		if f.debug {
			log.Printf("Prefetch: error fetching %s: %v", ftpPath, err)
		}
		return err
	}
	f.cacheMu.Lock()
	f.dirCache[ftpPath] = &DirCacheEntry{
		Files:     files,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()
	if f.debug {
		log.Printf("Prefetch: directory %s cached (%d entries)", ftpPath, len(files))
	}
	return nil
}

// PreloadDirCache preloads a directory cache with provided files
// Used to populate cache before FUSE starts serving requests
func (f *FtpFs) PreloadDirCache(ftpPath string, files []FtpFileInfo) {
	f.cacheMu.Lock()
	f.dirCache[ftpPath] = &DirCacheEntry{
		Files:     files,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()
	log.Printf("PreloadDirCache: %s preloaded with %d files", ftpPath, len(files))

	// Also create inodes for all files
	for i := range files {
		f.getOrCreateInode(1, &files[i])
	}
}

// SetUID sets the default UID for files
func (f *FtpFs) SetUID(uid uint32) {
	f.uid = uid
}

// SetGID sets the default GID for files
func (f *FtpFs) SetGID(gid uint32) {
	f.gid = gid
}

// Root returns the root node
func (f *FtpFs) Root() (fs.Node, error) {
	if f.debug {
		log.Printf("FUSE: Root() called - returning root inode %d", rootInode)
	}
	return &FtpNode{fs: f, inode: rootInode}, nil
}

// GenerateInode generates a dynamic inode number
func (f *FtpFs) GenerateInode(parentInode uint64, name string) uint64 {
	// Use hash of parent + name for stable inode
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%s", parentInode, name)
	ino := h.Sum64()
	if ino <= 1 {
		ino = 2
	}
	return ino
}

// allocateInode allocates a new inode number
func (f *FtpFs) allocateInode() uint64 {
	f.inodeMu.Lock()
	defer f.inodeMu.Unlock()
	ino := f.nextInode
	f.nextInode++
	return ino
}

// allocateFH allocates a new file handle
func (f *FtpFs) allocateFH() uint64 {
	f.handleMu.Lock()
	defer f.handleMu.Unlock()
	fh := f.nextFH
	f.nextFH++
	return fh
}

// getOrCreateInode gets or creates an inode for a file
func (f *FtpFs) getOrCreateInode(parent uint64, fileInfo *FtpFileInfo) *Inode {
	f.inodeMu.Lock()

	// Check if inode already exists
	if ino, ok := f.pathToInode[fileInfo.Path]; ok {
		if inode, ok := f.inodes[ino]; ok {
			f.inodeMu.Unlock()
			return inode
		}
	}
	f.inodeMu.Unlock()

	// Allocate new inode number (needs separate lock)
	ino := f.allocateInode()

	var mode os.FileMode
	if fileInfo.IsDir {
		mode = os.ModeDir | os.FileMode(fileInfo.Permissions)
	} else {
		mode = os.FileMode(fileInfo.Permissions)
	}

	attr := fuse.Attr{
		Inode: ino,
		Size:  fileInfo.Size,
		Mode:  mode,
		Uid:   f.uid,
		Gid:   f.gid,
		Atime: fileInfo.ModifiedTime,
		Mtime: fileInfo.ModifiedTime,
		Ctime: fileInfo.ModifiedTime,
	}

	inode := &Inode{
		Ino:     ino,
		Parent:  parent,
		Name:    fileInfo.Name,
		Attr:    attr,
		FtpPath: fileInfo.Path,
		IsDir:   fileInfo.IsDir,
	}

	// Need lock for maps
	f.inodeMu.Lock()
	f.inodes[ino] = inode
	f.pathToInode[fileInfo.Path] = ino
	f.inodeMu.Unlock()

	// Cache attributes
	f.cacheMu.Lock()
	f.attrCache[ino] = &AttrCacheEntry{
		Attr:      attr,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()

	return inode
}

// getInode returns an inode by number
func (f *FtpFs) getInode(ino uint64) *Inode {
	f.inodeMu.RLock()
	defer f.inodeMu.RUnlock()
	return f.inodes[ino]
}

// listDirectoryCached lists a directory with caching
// CRITICAL: This function must return quickly to avoid blocking FUSE.
func (f *FtpFs) listDirectoryCached(ftpPath string) ([]FtpFileInfo, error) {
	// ALWAYS check cache first - this is fast and never blocks
	f.cacheMu.RLock()
	if entry, ok := f.dirCache[ftpPath]; ok {
		// Cache valid for 60 seconds
		if time.Since(entry.Timestamp) < dirCacheTTL {
			f.cacheMu.RUnlock()
			if f.debug {
				log.Printf("listDirectoryCached: CACHE HIT for %s (%d files)", ftpPath, len(entry.Files))
			}
			return entry.Files, nil
		}
		// Cache expired but we can still use it while refreshing
		expiredEntry := entry
		f.cacheMu.RUnlock()

		// Trigger background refresh if not already running
		go f.backgroundRefresh(ftpPath)

		if f.debug {
			log.Printf("listDirectoryCached: returning STALE cache for %s while refreshing", ftpPath)
		}
		return expiredEntry.Files, nil
	}
	f.cacheMu.RUnlock()

	if f.debug {
		log.Printf("listDirectoryCached: CACHE MISS for %s", ftpPath)
	}

	// Check if there's already a fetch in progress for this path
	f.fetchMu.Lock()
	if fetch, ok := f.fetchMap[ftpPath]; ok {
		// Someone else is fetching this directory, wait for it
		f.fetchMu.Unlock()
		if f.debug {
			log.Printf("listDirectoryCached: waiting for existing fetch of %s", ftpPath)
		}
		<-fetch.ch
		if f.debug {
			log.Printf("listDirectoryCached: existing fetch completed for %s, got %d files", ftpPath, len(fetch.files))
		}
		return fetch.files, fetch.err
	}

	// Create a new fetch entry
	fetch := &fetchInProgress{
		ch: make(chan struct{}),
	}
	f.fetchMap[ftpPath] = fetch
	f.fetchMu.Unlock()

	// Do the actual fetch
	files, err := f.ftpConn.ListDir(ftpPath)

	// Store result and notify waiters
	fetch.files = files
	fetch.err = err
	close(fetch.ch)

	// Remove from fetch map
	f.fetchMu.Lock()
	delete(f.fetchMap, ftpPath)
	f.fetchMu.Unlock()

	if err != nil {
		if f.debug {
			log.Printf("listDirectoryCached: ERROR for %s: %v", ftpPath, err)
		}
		return nil, err
	}

	// Store in cache
	f.cacheMu.Lock()
	f.dirCache[ftpPath] = &DirCacheEntry{
		Files:     files,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()

	if f.debug {
		log.Printf("listDirectoryCached DONE: %s cached %d files", ftpPath, len(files))
	}
	return files, nil
}

// backgroundRefresh refreshes directory cache in background
func (f *FtpFs) backgroundRefresh(ftpPath string) {
	if f.debug {
		log.Printf("backgroundRefresh: starting for %s", ftpPath)
	}
	files, err := f.ftpConn.ListDir(ftpPath)
	if err != nil {
		if f.debug {
			log.Printf("backgroundRefresh: failed for %s: %v", ftpPath, err)
		}
		return
	}
	f.cacheMu.Lock()
	f.dirCache[ftpPath] = &DirCacheEntry{
		Files:     files,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()
	if f.debug {
		log.Printf("backgroundRefresh: completed for %s (%d files)", ftpPath, len(files))
	}
}

// invalidateDirCache invalidates a directory cache entry
func (f *FtpFs) invalidateDirCache(ftpPath string) {
	f.cacheMu.Lock()
	delete(f.dirCache, ftpPath)
	f.cacheMu.Unlock()
	if f.debug {
		log.Printf("Invalidated directory cache for: %s", ftpPath)
	}
}

// getAttrCached gets cached attributes
func (f *FtpFs) getAttrCached(ino uint64) *fuse.Attr {
	f.cacheMu.RLock()
	defer f.cacheMu.RUnlock()
	if entry, ok := f.attrCache[ino]; ok {
		if time.Since(entry.Timestamp) < attrCacheTTL {
			return &entry.Attr
		}
	}
	return nil
}

// updateAttrCache updates the attribute cache
func (f *FtpFs) updateAttrCache(ino uint64, attr *fuse.Attr) {
	f.cacheMu.Lock()
	f.attrCache[ino] = &AttrCacheEntry{
		Attr:      *attr,
		Timestamp: time.Now(),
	}
	f.cacheMu.Unlock()
}

// loadFileData loads file data with caching
func (f *FtpFs) loadFileData(ino uint64, ftpPath string) ([]byte, error) {
	start := time.Now()
	// Check cache first
	f.cacheMu.RLock()
	if data, ok := f.readCache[ino]; ok {
		f.cacheMu.RUnlock()
		if f.debug {
			log.Printf("File data cache hit for inode %d", ino)
		}
		return data, nil
	}
	f.cacheMu.RUnlock()

	// Load from FTP with timeout
	if f.debug {
		log.Printf("Loading file data for inode %d from FTP", ino)
	}

	type result struct {
		data []byte
		err  error
	}
	resChan := make(chan result, 1)
	go func() {
		data, err := f.ftpConn.Retrieve(ftpPath)
		resChan <- result{data: data, err: err}
	}()

	select {
	case res := <-resChan:
		if res.err != nil {
			return nil, res.err
		}
		// Store in cache
		f.cacheMu.Lock()
		f.readCache[ino] = res.data
		f.cacheMu.Unlock()

		if f.debug {
			log.Printf("File data loaded: %d bytes (took %v)", len(res.data), time.Since(start))
		}

		return res.data, nil
	case <-time.After(30 * time.Second):
		log.Printf("loadFileData: timeout loading file %s (inode %d)", ftpPath, ino)
		return nil, fmt.Errorf("timeout loading file")
	}
}

// syncWriteBuffer syncs a write buffer to FTP
func (f *FtpFs) syncWriteBuffer(fh uint64) error {
	start := time.Now()
	f.handleMu.RLock()
	fileHandle, ok := f.openFiles[fh]
	f.handleMu.RUnlock()

	if !ok || fileHandle.WriteBuffer == nil || !fileHandle.WriteBuffer.Dirty {
		return nil
	}

	inode := f.getInode(fileHandle.Ino)
	if inode == nil {
		return fmt.Errorf("inode not found")
	}

	if f.debug {
		log.Printf("Syncing write buffer for inode %d (%d bytes)",
			fileHandle.Ino, len(fileHandle.WriteBuffer.Data))
	}

	// Store with timeout
	type result struct {
		err error
	}
	resChan := make(chan result, 1)
	go func() {
		err := f.ftpConn.Store(inode.FtpPath, fileHandle.WriteBuffer.Data)
		resChan <- result{err: err}
	}()

	select {
	case res := <-resChan:
		if res.err != nil {
			return res.err
		}
	case <-time.After(30 * time.Second):
		log.Printf("syncWriteBuffer: timeout storing file %s", inode.FtpPath)
		return fmt.Errorf("timeout storing file")
	}

	// Update read cache
	f.cacheMu.Lock()
	f.readCache[fileHandle.Ino] = fileHandle.WriteBuffer.Data
	f.cacheMu.Unlock()

	// Update attributes
	attr := &inode.Attr
	attr.Size = uint64(len(fileHandle.WriteBuffer.Data))
	f.updateAttrCache(fileHandle.Ino, attr)

	// Invalidate parent directory cache
	if parent := f.getInode(inode.Parent); parent != nil {
		f.invalidateDirCache(parent.FtpPath)
	}

	fileHandle.WriteBuffer.Dirty = false
	if f.debug {
		log.Printf("Write buffer synced for %s (took %v)", inode.FtpPath, time.Since(start))
	}
	return nil
}

// FtpNode represents a node in the filesystem
type FtpNode struct {
	fs    *FtpFs
	inode uint64
}

// Attr fills the attribute structure
func (n *FtpNode) Attr(ctx context.Context, attr *fuse.Attr) error {
	if n.fs.debug {
		log.Printf("FUSE: Attr(inode=%d)", n.inode)
	}

	inode := n.fs.getInode(n.inode)
	if inode == nil {
		if n.fs.debug {
			log.Printf("FUSE: Attr(inode=%d) - inode not found!", n.inode)
		}
		return syscall.ENOENT
	}

	*attr = inode.Attr
	if n.fs.debug {
		log.Printf("FUSE: Attr(inode=%d) - mode=%v, size=%d", n.inode, attr.Mode, attr.Size)
	}
	return nil
}

// Lookup looks up a file by name
// CRITICAL: Must be fast! Uses only cache, never blocks on FTP.
func (n *FtpNode) Lookup(ctx context.Context, name string) (fs.Node, error) {
	start := time.Now()

	// Ignore temporary files (VS Code optimization)
	if isTempFile(name) {
		return nil, syscall.ENOENT
	}

	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return nil, syscall.ENOENT
	}

	// Handle special entries
	if name == "." {
		return &FtpNode{fs: n.fs, inode: n.inode}, nil
	}
	if name == ".." {
		return &FtpNode{fs: n.fs, inode: inode.Parent}, nil
	}

	// Build FTP path
	var ftpPath string
	if inode.FtpPath == "/" {
		ftpPath = "/" + name
	} else {
		ftpPath = inode.FtpPath + "/" + name
	}

	// FAST PATH 1: Check inode cache (never blocks)
	if ino, ok := n.fs.pathToInode[ftpPath]; ok {
		if cachedInode := n.fs.getInode(ino); cachedInode != nil {
			if n.fs.debug {
				log.Printf("FUSE-LOOKUP: inode cache hit for %s (took %v)", name, time.Since(start))
			}
			return &FtpNode{fs: n.fs, inode: ino}, nil
		}
	}

	// FAST PATH 2: Check directory cache (never blocks, 500ms timeout max)
	type dirResult struct {
		files []FtpFileInfo
		err   error
	}
	dirChan := make(chan dirResult, 1)
	go func() {
		files, err := n.fs.listDirectoryCached(inode.FtpPath)
		dirChan <- dirResult{files: files, err: err}
	}()

	select {
	case res := <-dirChan:
		if res.err == nil {
			for _, fileInfo := range res.files {
				if fileInfo.Name == name {
					childInode := n.fs.getOrCreateInode(n.inode, &fileInfo)
					if n.fs.debug {
						log.Printf("FUSE-LOOKUP: dir cache hit for %s (took %v)", name, time.Since(start))
					}
					return &FtpNode{fs: n.fs, inode: childInode.Ino}, nil
				}
			}
		}
	case <-time.After(500 * time.Millisecond):
		// Don't wait for slow FTP - directory cache will populate in background
		if n.fs.debug {
			log.Printf("FUSE-LOOKUP: FAST FAIL for %s (cache not ready, took %v)", name, time.Since(start))
		}
		return nil, syscall.ENOENT
	}

	// File not found in any cache
	if n.fs.debug {
		log.Printf("FUSE-LOOKUP: %s not found (took %v)", name, time.Since(start))
	}
	return nil, syscall.ENOENT
}

// ReadDirAll returns all directory entries
func (n *FtpNode) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return nil, syscall.ENOENT
	}

	if !inode.IsDir {
		return nil, syscall.ENOTDIR
	}

	if n.fs.debug {
		log.Printf("ReadDirAll: inode=%d path=%s", n.inode, inode.FtpPath)
	}

	// Get directory listing
	files, err := n.fs.listDirectoryCached(inode.FtpPath)
	if err != nil {
		return nil, syscall.EIO
	}

	// Build entries
	var entries []fuse.Dirent

	// Add . and ..
	entries = append(entries, fuse.Dirent{
		Inode: n.inode,
		Name:  ".",
		Type:  fuse.DT_Dir,
	})
	entries = append(entries, fuse.Dirent{
		Inode: inode.Parent,
		Name:  "..",
		Type:  fuse.DT_Dir,
	})

	// Add directory contents (filter temp files)
	for _, fileInfo := range files {
		if isTempFile(fileInfo.Name) {
			if n.fs.debug {
				log.Printf("ReadDirAll: filtering temp file %s", fileInfo.Name)
			}
			continue
		}

		childInode := n.fs.getOrCreateInode(n.inode, &fileInfo)

		var entryType fuse.DirentType
		if fileInfo.IsDir {
			entryType = fuse.DT_Dir
		} else {
			entryType = fuse.DT_File
		}

		entries = append(entries, fuse.Dirent{
			Inode: childInode.Ino,
			Name:  fileInfo.Name,
			Type:  entryType,
		})
	}

	return entries, nil
}

// Mkdir creates a directory
func (n *FtpNode) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return nil, syscall.ENOENT
	}

	if !inode.IsDir {
		return nil, syscall.ENOTDIR
	}

	var ftpPath string
	if inode.FtpPath == "/" {
		ftpPath = "/" + req.Name
	} else {
		ftpPath = inode.FtpPath + "/" + req.Name
	}

	if n.fs.debug {
		log.Printf("Mkdir: %s", ftpPath)
	}

	if err := n.fs.ftpConn.Mkdir(ftpPath); err != nil {
		return nil, syscall.EIO
	}

	// Invalidate cache
	n.fs.invalidateDirCache(inode.FtpPath)

	// Create inode
	fileInfo := &FtpFileInfo{
		Name:        req.Name,
		Path:        ftpPath,
		Size:        0,
		IsDir:       true,
		Permissions: 0755,
	}

	childInode := n.fs.getOrCreateInode(n.inode, fileInfo)
	return &FtpNode{fs: n.fs, inode: childInode.Ino}, nil
}

// Create creates a new file
func (n *FtpNode) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return nil, nil, syscall.ENOENT
	}

	if !inode.IsDir {
		return nil, nil, syscall.ENOTDIR
	}

	// Ignore temporary files (VS Code optimization)
	if isTempFile(req.Name) {
		if n.fs.debug {
			log.Printf("Create: ignoring temp file %s", req.Name)
		}
		return nil, nil, syscall.ENOTSUP
	}

	var ftpPath string
	if inode.FtpPath == "/" {
		ftpPath = "/" + req.Name
	} else {
		ftpPath = inode.FtpPath + "/" + req.Name
	}

	if n.fs.debug {
		log.Printf("Create: %s", ftpPath)
	}

	// Create empty file
	if err := n.fs.ftpConn.Store(ftpPath, []byte{}); err != nil {
		return nil, nil, syscall.EIO
	}

	// Invalidate cache
	n.fs.invalidateDirCache(inode.FtpPath)

	// Create inode
	fileInfo := &FtpFileInfo{
		Name:        req.Name,
		Path:        ftpPath,
		Size:        0,
		IsDir:       false,
		Permissions: 0644,
	}

	childInode := n.fs.getOrCreateInode(n.inode, fileInfo)
	node := &FtpNode{fs: n.fs, inode: childInode.Ino}

	// Create file handle with write buffer
	fh := n.fs.allocateFH()
	fileHandle := &FileHandle{
		Ino: childInode.Ino,
		WriteBuffer: &WriteBuffer{
			Data:  make([]byte, 0),
			Dirty: false,
		},
	}

	n.fs.handleMu.Lock()
	n.fs.openFiles[fh] = fileHandle
	n.fs.handleMu.Unlock()

	resp.Handle = fuse.HandleID(fh)
	return node, &FtpHandle{fs: n.fs, node: node, fh: fh, inode: childInode.Ino}, nil
}

// Remove removes a file or directory
func (n *FtpNode) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	if !inode.IsDir {
		return syscall.ENOTDIR
	}

	// Ignore temporary files
	if isTempFile(req.Name) {
		if n.fs.debug {
			log.Printf("Remove: ignoring temp file %s", req.Name)
		}
		return nil
	}

	var ftpPath string
	if inode.FtpPath == "/" {
		ftpPath = "/" + req.Name
	} else {
		ftpPath = inode.FtpPath + "/" + req.Name
	}

	if n.fs.debug {
		log.Printf("Remove: %s (dir=%v)", ftpPath, req.Dir)
	}

	// Check if exists first
	exists, _ := n.fs.ftpConn.Exists(ftpPath)
	if !exists {
		return nil // Already gone
	}

	var err error
	if req.Dir {
		err = n.fs.ftpConn.Rmdir(ftpPath)
	} else {
		err = n.fs.ftpConn.Delete(ftpPath)
	}

	if err != nil {
		return syscall.EIO
	}

	// Clean up caches
	if ino, ok := n.fs.pathToInode[ftpPath]; ok {
		n.fs.inodeMu.Lock()
		delete(n.fs.inodes, ino)
		n.fs.inodeMu.Unlock()

		n.fs.cacheMu.Lock()
		delete(n.fs.readCache, ino)
		delete(n.fs.attrCache, ino)
		n.fs.cacheMu.Unlock()
	}

	n.fs.inodeMu.Lock()
	delete(n.fs.pathToInode, ftpPath)
	n.fs.inodeMu.Unlock()

	// Invalidate directory cache
	n.fs.invalidateDirCache(inode.FtpPath)

	return nil
}

// Rename renames a file
func (n *FtpNode) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	newDirNode, ok := newDir.(*FtpNode)
	if !ok {
		return syscall.EINVAL
	}

	newDirInode := n.fs.getInode(newDirNode.inode)
	if newDirInode == nil {
		return syscall.ENOENT
	}

	var oldPath, newPath string
	if inode.FtpPath == "/" {
		oldPath = "/" + req.OldName
	} else {
		oldPath = inode.FtpPath + "/" + req.OldName
	}

	if newDirInode.FtpPath == "/" {
		newPath = "/" + req.NewName
	} else {
		newPath = newDirInode.FtpPath + "/" + req.NewName
	}

	if n.fs.debug {
		log.Printf("Rename: %s -> %s", oldPath, newPath)
	}

	if err := n.fs.ftpConn.Rename(oldPath, newPath); err != nil {
		return syscall.EIO
	}

	// Update inode cache
	if ino, ok := n.fs.pathToInode[oldPath]; ok {
		n.fs.inodeMu.Lock()
		if inode, ok := n.fs.inodes[ino]; ok {
			inode.FtpPath = newPath
			inode.Name = req.NewName
			inode.Parent = newDirNode.inode
		}
		delete(n.fs.pathToInode, oldPath)
		n.fs.pathToInode[newPath] = ino
		n.fs.inodeMu.Unlock()
	}

	// Invalidate caches
	n.fs.invalidateDirCache(inode.FtpPath)
	if inode.FtpPath != newDirInode.FtpPath {
		n.fs.invalidateDirCache(newDirInode.FtpPath)
	}

	return nil
}

// Setattr sets attributes
func (n *FtpNode) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	attr := &inode.Attr

	if req.Valid.Mode() {
		attr.Mode = req.Mode
	}
	if req.Valid.Uid() {
		attr.Uid = req.Uid
	}
	if req.Valid.Gid() {
		attr.Gid = req.Gid
	}
	if req.Valid.Size() {
		attr.Size = req.Size
	}

	resp.Attr = *attr
	n.fs.updateAttrCache(n.inode, attr)

	return nil
}

// Getattr gets attributes
func (n *FtpNode) Getattr(ctx context.Context, req *fuse.GetattrRequest, resp *fuse.GetattrResponse) error {
	// Try cache first
	if attr := n.fs.getAttrCached(n.inode); attr != nil {
		resp.Attr = *attr
		return nil
	}

	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	// For regular files, update size occasionally
	if !inode.IsDir {
		if info, err := n.fs.ftpConn.GetFileInfo(inode.FtpPath); err == nil {
			inode.Attr.Size = info.Size
			n.fs.updateAttrCache(n.inode, &inode.Attr)
		}
	}

	resp.Attr = inode.Attr
	n.fs.updateAttrCache(n.inode, &inode.Attr)

	return nil
}

// Open opens a file or directory
func (n *FtpNode) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	inode := n.fs.getInode(n.inode)
	if inode == nil {
		return nil, syscall.ENOENT
	}

	if n.fs.debug {
		log.Printf("Open: inode=%d path=%s flags=%d isDir=%v", n.inode, inode.FtpPath, req.Flags, inode.IsDir)
	}

	// Allocate file handle
	fh := n.fs.allocateFH()

	// Check if write mode (only for files)
	isWriteMode := false
	if !inode.IsDir {
		isWriteMode = (req.Flags&0x01) != 0 || (req.Flags&0x02) != 0
	}

	fileHandle := &FileHandle{
		Ino: n.inode,
	}

	if isWriteMode {
		fileHandle.WriteBuffer = &WriteBuffer{
			Data:  make([]byte, 0),
			Dirty: false,
		}
	}

	n.fs.handleMu.Lock()
	n.fs.openFiles[fh] = fileHandle
	n.fs.handleMu.Unlock()

	resp.Handle = fuse.HandleID(fh)
	return &FtpHandle{fs: n.fs, node: n, fh: fh, inode: n.inode, isDir: inode.IsDir}, nil
}

// FtpHandle represents an open file handle
type FtpHandle struct {
	fs    *FtpFs
	node  *FtpNode
	fh    uint64
	inode uint64
	isDir bool
}

// Read reads data from the file
func (h *FtpHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	inode := h.fs.getInode(h.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	if inode.IsDir {
		return syscall.EISDIR
	}

	if h.fs.debug {
		log.Printf("Read: inode=%d offset=%d size=%d", h.inode, req.Offset, req.Size)
	}

	// Load file data
	data, err := h.fs.loadFileData(h.inode, inode.FtpPath)
	if err != nil {
		return syscall.EIO
	}

	offset := int(req.Offset)
	if offset >= len(data) {
		resp.Data = []byte{}
		return nil
	}

	end := offset + int(req.Size)
	if end > len(data) {
		end = len(data)
	}

	resp.Data = data[offset:end]
	return nil
}

// Write writes data to the file
func (h *FtpHandle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	inode := h.fs.getInode(h.inode)
	if inode == nil {
		return syscall.ENOENT
	}

	if inode.IsDir {
		return syscall.EISDIR
	}

	if h.fs.debug {
		log.Printf("Write: inode=%d offset=%d size=%d", h.inode, req.Offset, len(req.Data))
	}

	// Get file handle
	h.fs.handleMu.Lock()
	fileHandle, ok := h.fs.openFiles[h.fh]
	h.fs.handleMu.Unlock()

	if !ok || fileHandle.WriteBuffer == nil {
		return syscall.EIO
	}

	// Expand buffer if needed
	offset := int(req.Offset)
	end := offset + len(req.Data)
	if end > len(fileHandle.WriteBuffer.Data) {
		newData := make([]byte, end)
		copy(newData, fileHandle.WriteBuffer.Data)
		fileHandle.WriteBuffer.Data = newData
	}

	// Write data
	copy(fileHandle.WriteBuffer.Data[offset:end], req.Data)
	fileHandle.WriteBuffer.Dirty = true
	fileHandle.WriteBuffer.LastModified = time.Now()

	// Update read cache
	h.fs.cacheMu.Lock()
	h.fs.readCache[h.inode] = fileHandle.WriteBuffer.Data
	h.fs.cacheMu.Unlock()

	resp.Size = len(req.Data)
	return nil
}

// Flush flushes pending writes
func (h *FtpHandle) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	if h.fs.debug {
		log.Printf("Flush: fh=%d", h.fh)
	}
	return h.fs.syncWriteBuffer(h.fh)
}

// Release releases the file handle
func (h *FtpHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if h.fs.debug {
		log.Printf("Release: fh=%d inode=%d", h.fh, h.inode)
	}

	// Sync write buffer
	if err := h.fs.syncWriteBuffer(h.fh); err != nil {
		log.Printf("Warning: failed to sync write buffer: %v", err)
	}

	// Remove file handle
	h.fs.handleMu.Lock()
	delete(h.fs.openFiles, h.fh)
	h.fs.handleMu.Unlock()

	// Clean up read cache if no other handles
	h.fs.handleMu.RLock()
	hasOtherHandles := false
	for _, handle := range h.fs.openFiles {
		if handle.Ino == h.inode {
			hasOtherHandles = true
			break
		}
	}
	h.fs.handleMu.RUnlock()

	if !hasOtherHandles {
		h.fs.cacheMu.Lock()
		delete(h.fs.readCache, h.inode)
		h.fs.cacheMu.Unlock()
	}

	return nil
}

// Fsync syncs file data
func (h *FtpHandle) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	if h.fs.debug {
		log.Printf("Fsync: fh=%d", h.fh)
	}
	return h.fs.syncWriteBuffer(h.fh)
}

// ReadDirAll reads all directory entries (for directories)
func (h *FtpHandle) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	if !h.isDir {
		return nil, syscall.ENOTDIR
	}

	inode := h.fs.getInode(h.inode)
	if inode == nil {
		return nil, syscall.ENOENT
	}

	if h.fs.debug {
		log.Printf("FUSE ReadDirAll: inode=%d path=%s", h.inode, inode.FtpPath)
	}

	// Get directory listing
	files, err := h.fs.listDirectoryCached(inode.FtpPath)
	if err != nil {
		if h.fs.debug {
			log.Printf("FUSE ReadDirAll ERROR: inode=%d path=%s err=%v", h.inode, inode.FtpPath, err)
		}
		return nil, syscall.EIO
	}

	if h.fs.debug {
		log.Printf("FUSE ReadDirAll: got %d files for %s", len(files), inode.FtpPath)
	}

	// Build entries
	var entries []fuse.Dirent

	// Add . and ..
	entries = append(entries, fuse.Dirent{
		Inode: h.inode,
		Name:  ".",
		Type:  fuse.DT_Dir,
	})
	entries = append(entries, fuse.Dirent{
		Inode: inode.Parent,
		Name:  "..",
		Type:  fuse.DT_Dir,
	})

	// Add directory contents (filter temp files)
	for _, fileInfo := range files {
		if isTempFile(fileInfo.Name) {
			if h.fs.debug {
				log.Printf("ReadDirAll: filtering temp file %s", fileInfo.Name)
			}
			continue
		}

		childInode := h.fs.getOrCreateInode(h.inode, &fileInfo)

		var entryType fuse.DirentType
		if fileInfo.IsDir {
			entryType = fuse.DT_Dir
		} else {
			entryType = fuse.DT_File
		}

		entries = append(entries, fuse.Dirent{
			Inode: childInode.Ino,
			Name:  fileInfo.Name,
			Type:  entryType,
		})
	}

	return entries, nil
}

// Ensure interfaces are implemented
var _ = fs.Node(&FtpNode{})
var _ = fs.NodeStringLookuper(&FtpNode{})
var _ = fs.NodeMkdirer(&FtpNode{})
var _ = fs.NodeCreater(&FtpNode{})
var _ = fs.NodeRemover(&FtpNode{})
var _ = fs.NodeRenamer(&FtpNode{})
var _ = fs.NodeSetattrer(&FtpNode{})
var _ = fs.NodeGetattrer(&FtpNode{})
var _ = fs.NodeOpener(&FtpNode{})
var _ = fs.Handle(&FtpHandle{})
var _ = fs.HandleReader(&FtpHandle{})
var _ = fs.HandleWriter(&FtpHandle{})
var _ = fs.HandleFlusher(&FtpHandle{})
var _ = fs.HandleReleaser(&FtpHandle{})
var _ = fs.HandleReadDirAller(&FtpHandle{})
