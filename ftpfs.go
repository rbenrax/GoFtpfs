package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/textproto"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/jlaffaye/ftp"
	"golang.org/x/sync/singleflight"
)

// FTPFS representa el sistema de archivos FTP
type FTPFS struct {
	host     string
	port     int
	user     string
	password string
	conn     *ftp.ServerConn
	mu       sync.RWMutex
	cache    *Cache
	group    singleflight.Group
}

// Cache para directorios y archivos
type Cache struct {
	dirEntries map[string][]fuse.Dirent
	fileData   map[string][]byte
	mu         sync.RWMutex
	ttl        time.Duration
}

// Archivo representa un archivo en el sistema FTP
type Archivo struct {
	fs     *FTPFS
	path   string
	mu     sync.RWMutex
	buffer []byte
	isDirty bool
}

// Directorio representa un directorio en el sistema FTP
type Directorio struct {
	fs   *FTPFS
	path string
}

// Config configuración del sistema de archivos
type Config struct {
	Host     string
	Port     int
	User     string
	Password string
	CacheTTL time.Duration
}

// NuevoFTPFS crea una nueva instancia del sistema de archivos FTP
func NuevoFTPFS(cfg Config) (*FTPFS, error) {
	// Conectar al servidor FTP
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	conn, err := ftp.Dial(addr, ftp.DialWithTimeout(30*time.Second))
	if err != nil {
		return nil, fmt.Errorf("error conectando a FTP: %w", err)
	}

	// Login
	if err := conn.Login(cfg.User, cfg.Password); err != nil {
		return nil, fmt.Errorf("error de login: %w", err)
	}

	return &FTPFS{
		host:     cfg.Host,
		port:     cfg.Port,
		user:     cfg.User,
		password: cfg.Password,
		conn:     conn,
		cache: &Cache{
			dirEntries: make(map[string][]fuse.Dirent),
			fileData:   make(map[string][]byte),
			ttl:        cfg.CacheTTL,
		},
	}, nil
}

// Reconectar reconecta al servidor FTP si es necesario
func (f *FTPFS) Reconectar() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.conn != nil {
		f.conn.Quit()
	}

	addr := fmt.Sprintf("%s:%d", f.host, f.port)
	conn, err := ftp.Dial(addr, ftp.DialWithTimeout(30*time.Second))
	if err != nil {
		return err
	}

	if err := conn.Login(f.user, f.password); err != nil {
		return err
	}

	f.conn = conn
	f.cache.mu.Lock()
	f.cache.dirEntries = make(map[string][]fuse.Dirent)
	f.cache.fileData = make(map[string][]byte)
	f.cache.mu.Unlock()

	return nil
}

// ObtenerConexion obtiene una conexión FTP (con reconexión automática)
func (f *FTPFS) ObtenerConexion() (*ftp.ServerConn, error) {
	f.mu.RLock()
	conn := f.conn
	f.mu.RUnlock()

	// Verificar si la conexión está activa
	if conn != nil {
		if _, err := conn.CurrentDir(); err == nil {
			return conn, nil
		}
	}

	// Reconectar si es necesario
	if err := f.Reconectar(); err != nil {
		return nil, err
	}

	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.conn, nil
}

// Root implementa fs.FS
func (f *FTPFS) Root() (fs.Node, error) {
	return &Directorio{fs: f, path: "/"}, nil
}

// ========== IMPLEMENTACIÓN DE DIRECTORIO ==========

// Attr retorna los atributos del directorio
func (d *Directorio) Attr(ctx context.Context, a *fuse.Attr) error {
	a.Mode = os.ModeDir | 0755
	a.Uid = uint32(os.Getuid())
	a.Gid = uint32(os.Getgid())
	a.Valid = 10 * time.Second
	return nil
}

// Lookup busca un archivo o directorio
func (d *Directorio) Lookup(ctx context.Context, name string) (fs.Node, error) {
	fullPath := path.Join(d.path, name)
	
	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return nil, fuse.ENOENT
	}

	// Intentar listar como directorio primero
	entries, err := conn.List(fullPath)
	if err == nil && len(entries) > 0 {
		return &Directorio{fs: d.fs, path: fullPath}, nil
	}

	// Intentar obtener como archivo
	_, err = conn.FileSize(fullPath)
	if err != nil {
		return nil, fuse.ENOENT
	}

	return &Archivo{fs: d.fs, path: fullPath}, nil
}

// ReadDirAll lee todos los elementos del directorio
func (d *Directorio) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	// Verificar cache primero
	d.fs.cache.mu.RLock()
	if entries, ok := d.fs.cache.dirEntries[d.path]; ok {
		d.fs.cache.mu.RUnlock()
		return entries, nil
	}
	d.fs.cache.mu.RUnlock()

	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return nil, fuse.EIO
	}

	// Usar singleflight para evitar listados duplicados
	key := "dir:" + d.path
	entries, err, _ := d.fs.group.Do(key, func() (interface{}, error) {
		var dirents []fuse.Dirent
		
		// Listar directorio
		listPath := d.path
		if listPath == "/" {
			listPath = "."
		}
		
		ftpEntries, err := conn.List(listPath)
		if err != nil {
			return nil, err
		}

		for _, entry := range ftpEntries {
			if entry.Name == "." || entry.Name == ".." {
				continue
			}

			dirent := fuse.Dirent{
				Name: entry.Name,
			}

			switch entry.Type {
			case ftp.EntryTypeFolder:
				dirent.Type = fuse.DT_Dir
			case ftp.EntryTypeLink:
				dirent.Type = fuse.DT_Link
			default:
				dirent.Type = fuse.DT_File
			}

			dirents = append(dirents, dirent)
		}

		// Guardar en cache
		d.fs.cache.mu.Lock()
		d.fs.cache.dirEntries[d.path] = dirents
		d.fs.cache.mu.Unlock()

		return dirents, nil
	})

	if err != nil {
		return nil, fuse.EIO
	}

	return entries.([]fuse.Dirent), nil
}

// Create crea un nuevo archivo
func (d *Directorio) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {
	fullPath := path.Join(d.path, req.Name)
	
	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return nil, nil, fuse.EIO
	}

	// Crear archivo vacío
	data := bytes.NewReader([]byte{})
	err = conn.Stor(fullPath, data)
	if err != nil {
		return nil, nil, fuse.EIO
	}

	// Invalidar cache del directorio
	d.fs.cache.mu.Lock()
	delete(d.fs.cache.dirEntries, d.path)
	d.fs.cache.mu.Unlock()

	archivo := &Archivo{fs: d.fs, path: fullPath}
	return archivo, archivo, nil
}

// Mkdir crea un nuevo directorio
func (d *Directorio) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {
	fullPath := path.Join(d.path, req.Name)
	
	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return nil, fuse.EIO
	}

	err = conn.MakeDir(fullPath)
	if err != nil {
		return nil, fuse.EIO
	}

	// Invalidar cache del directorio padre
	d.fs.cache.mu.Lock()
	delete(d.fs.cache.dirEntries, d.path)
	d.fs.cache.mu.Unlock()

	return &Directorio{fs: d.fs, path: fullPath}, nil
}

// Remove elimina un archivo o directorio
func (d *Directorio) Remove(ctx context.Context, req *fuse.RemoveRequest) error {
	fullPath := path.Join(d.path, req.Name)
	
	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	var err error
	if req.Dir {
		err = conn.RemoveDir(fullPath)
	} else {
		err = conn.Delete(fullPath)
	}

	if err != nil {
		return fuse.EIO
	}

	// Invalidar cache
	d.fs.cache.mu.Lock()
	delete(d.fs.cache.dirEntries, d.path)
	delete(d.fs.cache.fileData, fullPath)
	d.fs.cache.mu.Unlock()

	return nil
}

// Rename renombra un archivo o directorio
func (d *Directorio) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {
	oldPath := path.Join(d.path, req.OldName)
	newParent := newDir.(*Directorio)
	newPath := path.Join(newParent.path, req.NewName)
	
	conn, err := d.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	err = conn.Rename(oldPath, newPath)
	if err != nil {
		return fuse.EIO
	}

	// Invalidar cache
	d.fs.cache.mu.Lock()
	delete(d.fs.cache.dirEntries, d.path)
	delete(d.fs.cache.dirEntries, newParent.path)
	delete(d.fs.cache.fileData, oldPath)
	d.fs.cache.mu.Unlock()

	return nil
}

// ========== IMPLEMENTACIÓN DE ARCHIVO ==========

// Attr retorna los atributos del archivo
func (a *Archivo) Attr(ctx context.Context, attr *fuse.Attr) error {
	conn, err := a.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	size, err := conn.FileSize(a.path)
	if err != nil {
		return fuse.ENOENT
	}

	attr.Mode = 0644
	attr.Size = uint64(size)
	attr.Uid = uint32(os.Getuid())
	attr.Gid = uint32(os.Getgid())
	attr.Valid = 10 * time.Second
	
	return nil
}

// Read lee datos del archivo
func (a *Archivo) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Si hay datos en buffer, leer desde allí
	if a.isDirty && len(a.buffer) > 0 {
		end := req.Offset + int64(req.Size)
		if end > int64(len(a.buffer)) {
			end = int64(len(a.buffer))
		}
		if req.Offset >= int64(len(a.buffer)) {
			resp.Data = []byte{}
			return nil
		}
		resp.Data = make([]byte, end-req.Offset)
		copy(resp.Data, a.buffer[req.Offset:end])
		return nil
	}

	// Leer desde FTP
	conn, err := a.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	r, err := conn.Retr(a.path)
	if err != nil {
		return fuse.EIO
	}
	defer r.Close()

	// Leer todo el contenido
	data, err := io.ReadAll(r)
	if err != nil {
		return fuse.EIO
	}

	// Verificar límites de lectura
	if req.Offset >= int64(len(data)) {
		resp.Data = []byte{}
		return nil
	}

	end := req.Offset + int64(req.Size)
	if end > int64(len(data)) {
		end = int64(len(data))
	}

	resp.Data = make([]byte, end-req.Offset)
	copy(resp.Data, data[req.Offset:end])
	
	return nil
}

// Write escribe datos en el archivo
func (a *Archivo) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Asegurar que el buffer es suficientemente grande
	requiredSize := int(req.Offset) + len(req.Data)
	if requiredSize > len(a.buffer) {
		newBuffer := make([]byte, requiredSize)
		copy(newBuffer, a.buffer)
		a.buffer = newBuffer
	}

	// Escribir datos en buffer
	copy(a.buffer[req.Offset:], req.Data)
	a.isDirty = true

	resp.Size = len(req.Data)
	return nil
}

// Flush sincroniza los datos - IMPORTANTE para VS Code
func (a *Archivo) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.isDirty {
		return nil
	}

	// Subir datos a FTP
	conn, err := a.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	data := bytes.NewReader(a.buffer)
	err = conn.Stor(a.path, data)
	if err != nil {
		return fuse.EIO
	}

	// Invalidar cache
	a.fs.cache.mu.Lock()
	delete(a.fs.cache.fileData, a.path)
	delete(a.fs.cache.dirEntries, filepath.Dir(a.path))
	a.fs.cache.mu.Unlock()

	a.isDirty = false
	return nil
}

// Setattr establece atributos del archivo
func (a *Archivo) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	if req.Valid.Size() {
		a.mu.Lock()
		defer a.mu.Unlock()

		if int(req.Size) < len(a.buffer) {
			a.buffer = a.buffer[:req.Size]
		} else {
			newBuffer := make([]byte, req.Size)
			copy(newBuffer, a.buffer)
			a.buffer = newBuffer
		}
		a.isDirty = true
	}
	return nil
}

// Fsync sincronización forzada - CRÍTICO para VS Code
func (a *Archivo) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return a.Flush(ctx, &fuse.FlushRequest{})
}

// ========== FUNCIÓN PRINCIPAL ==========

func main() {
	// Configuración por defecto
	config := Config{
		Host:     "localhost",
		Port:     21,
		User:     "anonymous",
		Password: "",
		CacheTTL: 30 * time.Second,
	}

	// Parsear argumentos
	if len(os.Args) < 3 {
		fmt.Printf("Uso: %s <host> <punto_montaje> [opciones]\n", os.Args[0])
		fmt.Println("Opciones:")
		fmt.Println("  -p, --port PORT      Puerto FTP (default: 21)")
		fmt.Println("  -u, --user USER      Usuario FTP (default: anonymous)")
		fmt.Println("  -P, --password PASS  Contraseña FTP")
		fmt.Println("  -d, --debug          Modo debug")
		fmt.Println("  -f, --foreground     Ejecutar en primer plano")
		os.Exit(1)
	}

	config.Host = os.Args[1]
	mountpoint := os.Args[2]

	// Parsear opciones restantes
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "-p", "--port":
			i++
			if i < len(os.Args) {
				port, err := strconv.Atoi(os.Args[i])
				if err != nil {
					log.Fatalf("Puerto inválido: %v", err)
				}
				config.Port = port
			}
		case "-u", "--user":
			i++
			if i < len(os.Args) {
				config.User = os.Args[i]
			}
		case "-P", "--password":
			i++
			if i < len(os.Args) {
				config.Password = os.Args[i]
			}
		case "-d", "--debug":
			// El debug se maneja con FUSE
		case "-f", "--foreground":
			// Por defecto ya está en foreground
		}
	}

	// Verificar punto de montaje
	if _, err := os.Stat(mountpoint); os.IsNotExist(err) {
		log.Fatalf("El punto de montaje %s no existe", mountpoint)
	}

	fmt.Printf("Montando %s:%d en %s\n", config.Host, config.Port, mountpoint)
	fmt.Printf("Usuario: %s\n", config.User)
	fmt.Println("Para desmontar: fusermount -u", mountpoint)

	// Crear sistema de archivos FTP
	ftpFS, err := NuevoFTPFS(config)
	if err != nil {
		log.Fatalf("Error creando FTPFS: %v", err)
	}
	defer ftpFS.conn.Quit()

	// Configurar FUSE
	fuseConfig := []fuse.MountOption{
		fuse.FSName("ftpfs"),
		fuse.Subtype("ftpfs"),
		fuse.LocalVolume(),
		fuse.VolumeName("FTP:" + config.Host),
		fuse.AllowOther(),
	}

	// Montar sistema de archivos
	c, err := fuse.Mount(mountpoint, fuseConfig...)
	if err != nil {
		log.Fatalf("Error montando FUSE: %v", err)
	}
	defer c.Close()

	// Manejar señales para desmontar correctamente
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	
	go func() {
		<-sigCh
		fmt.Println("\nDesmontando...")
		fuse.Unmount(mountpoint)
		os.Exit(0)
	}()

	// Servir sistema de archivos
	err = fs.Serve(c, ftpFS)
	if err != nil {
		log.Fatalf("Error sirviendo FUSE: %v", err)
	}

	// Verificar si el mount fue exitoso
	<-c.Ready
	if err := c.MountError; err != nil {
		log.Fatalf("Error de montaje: %v", err)
	}
}
