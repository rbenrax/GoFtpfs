// Package ftpfs implements a FUSE filesystem for FTP servers
// This is a simplified single-connection version without pooling
package ftpfs

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"path"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// FtpFileInfo represents information about a file or directory on the FTP server
type FtpFileInfo struct {
	Name         string
	Path         string
	Size         uint64
	IsDir        bool
	Permissions  uint32
	ModifiedTime time.Time
}

// SimpleFTPConnection manages a single FTP connection with mutual exclusion
type SimpleFTPConnection struct {
	conn     *ftp.ServerConn
	mu       sync.Mutex // Protects conn
	server   string
	username string
	password string
	useTLS   bool
	port     int
	debug    bool
}

// NewSimpleFTPConnection creates a new FTP connection
func NewSimpleFTPConnection(server, username, password string, useTLS bool, port int, debug bool) (*SimpleFTPConnection, error) {
	if port == 0 {
		port = 21
	}

	addr := fmt.Sprintf("%s:%d", server, port)
	log.Printf("Connecting to FTP server at %s", addr)

	var conn *ftp.ServerConn
	var err error

	if useTLS {
		config := &tls.Config{InsecureSkipVerify: true}
		if debug {
			log.Printf("FTP: Dialing with TLS to %s...", addr)
		}
		conn, err = ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second), ftp.DialWithTLS(config))
	} else {
		if debug {
			log.Printf("FTP: Dialing to %s (timeout 5s)...", addr)
		}
		conn, err = ftp.Dial(addr, ftp.DialWithTimeout(5*time.Second), ftp.DialWithDisabledMLSD(true))
	}

	if err != nil {
		log.Printf("FTP: Connection failed: %v", err)
		return nil, fmt.Errorf("failed to connect to FTP server: %v", err)
	}

	if debug {
		log.Printf("FTP: Connected, logging in...")
	}

	if err := conn.Login(username, password); err != nil {
		log.Printf("FTP: Login failed: %v", err)
		conn.Quit()
		return nil, fmt.Errorf("failed to login: %v", err)
	}

	if err := conn.Type(ftp.TransferTypeBinary); err != nil {
		conn.Quit()
		return nil, fmt.Errorf("failed to set binary mode: %v", err)
	}

	log.Printf("Successfully connected to FTP server")

	return &SimpleFTPConnection{
		conn:     conn,
		server:   server,
		username: username,
		password: password,
		useTLS:   useTLS,
		port:     port,
		debug:    debug,
	}, nil
}

// ListDir lists files in a directory with timeout protection
func (c *SimpleFTPConnection) ListDir(dirPath string) ([]FtpFileInfo, error) {
	if c.debug {
		log.Printf("FTP ListDir: %s", dirPath)
	}

	// Use a simple timeout approach without goroutines to avoid complexity
	type result struct {
		entries []*ftp.Entry
		err     error
	}

	resChan := make(chan result, 1)

	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		entries, err := c.conn.List(dirPath)
		resChan <- result{entries: entries, err: err}
	}()

	select {
	case res := <-resChan:
		if res.err != nil {
			if c.debug {
				log.Printf("FTP ListDir ERROR for %s: %v", dirPath, res.err)
			}
			return nil, res.err
		}
		if c.debug {
			log.Printf("FTP ListDir: %s got %d entries", dirPath, len(res.entries))
		}
		return c.parseEntries(res.entries, dirPath), nil
	case <-time.After(15 * time.Second):
		log.Printf("FTP ListDir TIMEOUT for %s after 15s", dirPath)
		return nil, fmt.Errorf("ListDir timeout after 15s for %s", dirPath)
	}
}

// Pwd returns current directory
func (c *SimpleFTPConnection) Pwd() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.CurrentDir()
}

// Cwd changes working directory
func (c *SimpleFTPConnection) Cwd(dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.ChangeDir(dir)
}

// Cdup changes to parent directory
func (c *SimpleFTPConnection) Cdup() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.ChangeDirToParent()
}

// List lists current directory
func (c *SimpleFTPConnection) List() ([]FtpFileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := c.conn.List("")
	if err != nil {
		return nil, err
	}

	return c.parseEntries(entries, ""), nil
}

// Size returns file size
func (c *SimpleFTPConnection) Size(filePath string) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	size, err := c.conn.FileSize(filePath)
	if err != nil {
		return 0, err
	}
	return uint64(size), nil
}

// Retrieve downloads file contents
func (c *SimpleFTPConnection) Retrieve(filePath string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	reader, err := c.conn.Retr(filePath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

// Store uploads file contents
func (c *SimpleFTPConnection) Store(filePath string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	reader := bytes.NewReader(data)
	return c.conn.Stor(filePath, reader)
}

// Delete removes a file
func (c *SimpleFTPConnection) Delete(filePath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.conn.Delete(filePath)
}

// Mkdir creates a directory
func (c *SimpleFTPConnection) Mkdir(dirPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.conn.MakeDir(dirPath)
}

// Rmdir removes a directory
func (c *SimpleFTPConnection) Rmdir(dirPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.conn.RemoveDir(dirPath)
}

// Rename renames a file/directory
func (c *SimpleFTPConnection) Rename(oldPath, newPath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.conn.Rename(oldPath, newPath)
}

// IsDir checks if path is a directory
func (c *SimpleFTPConnection) IsDir(filePath string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	currentDir, err := c.conn.CurrentDir()
	if err != nil {
		return false, err
	}

	err = c.conn.ChangeDir(filePath)
	if err != nil {
		c.conn.ChangeDir(currentDir)
		return false, nil
	}

	c.conn.ChangeDir(currentDir)
	return true, nil
}

// Exists checks if file/directory exists
func (c *SimpleFTPConnection) Exists(filePath string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, err := c.conn.FileSize(filePath)
	if err == nil {
		return true, nil
	}

	currentDir, _ := c.conn.CurrentDir()
	err = c.conn.ChangeDir(filePath)
	if err != nil {
		c.conn.ChangeDir(currentDir)
		return false, nil
	}

	c.conn.ChangeDir(currentDir)
	return true, nil
}

// GetFileInfo returns file information
func (c *SimpleFTPConnection) GetFileInfo(filePath string) (*FtpFileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	parentDir := path.Dir(filePath)
	fileName := path.Base(filePath)

	if parentDir == "." || parentDir == "" {
		parentDir = "/"
	}

	entries, err := c.conn.List(parentDir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.Name == fileName {
			return &FtpFileInfo{
				Name:         entry.Name,
				Path:         filePath,
				Size:         entry.Size,
				IsDir:        entry.Type == ftp.EntryTypeFolder,
				ModifiedTime: entry.Time,
			}, nil
		}
	}

	return nil, fmt.Errorf("file not found")
}

// Reconnect attempts to reconnect to FTP server
func (c *SimpleFTPConnection) Reconnect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		c.conn.Quit()
	}

	addr := fmt.Sprintf("%s:%d", c.server, c.port)
	var conn *ftp.ServerConn
	var err error

	if c.useTLS {
		config := &tls.Config{InsecureSkipVerify: true}
		conn, err = ftp.Dial(addr, ftp.DialWithTimeout(10*time.Second), ftp.DialWithTLS(config))
	} else {
		conn, err = ftp.Dial(addr, ftp.DialWithTimeout(10*time.Second), ftp.DialWithDisabledMLSD(true))
	}

	if err != nil {
		return err
	}

	if err := conn.Login(c.username, c.password); err != nil {
		conn.Quit()
		return err
	}

	if err := conn.Type(ftp.TransferTypeBinary); err != nil {
		conn.Quit()
		return err
	}

	c.conn = conn
	return nil
}

// Close closes the connection
func (c *SimpleFTPConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return c.conn.Quit()
	}
	return nil
}

// parseEntries converts FTP entries to FtpFileInfo
func (c *SimpleFTPConnection) parseEntries(entries []*ftp.Entry, currentDir string) []FtpFileInfo {
	var files []FtpFileInfo

	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}

		fullPath := path.Join(currentDir, entry.Name)
		if fullPath == "/" || fullPath == "" {
			fullPath = entry.Name
		}

		info := FtpFileInfo{
			Name:         entry.Name,
			Path:         fullPath,
			Size:         entry.Size,
			IsDir:        entry.Type == ftp.EntryTypeFolder,
			ModifiedTime: entry.Time,
		}

		// Set permissions
		if info.IsDir {
			info.Permissions = 0755
		} else {
			info.Permissions = 0644
		}

		files = append(files, info)
	}

	return files
}
