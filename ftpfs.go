package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
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
	tempDir  string
}

// Cache para directorios y archivos
type Cache struct {
	dirEntries map[string][]fuse.Dirent
	fileData   map[string][]byte
	mu         sync.RWMutex
	ttl        time.Duration
}

// Archivo representa un archivo en el sistema FTP con cache temporal
type Archivo struct {
	fs       *FTPFS
	path     string
	mu       sync.RWMutex
	tempFile string
	isDirty  bool
	isOpen   bool
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

	// Crear directorio temporal
	tempDir, err := os.MkdirTemp("", "ftpfs_*")
	if err != nil {
		return nil, fmt.Errorf("error creando directorio temporal: %w", err)
	}

	log.Printf("Directorio temporal creado: %s", tempDir)

	return &FTPFS{
		host:     cfg.Host,
		port:     cfg.Port,
		user:     cfg.User,
		password: cfg.Password,
		conn:     conn,
		tempDir:  tempDir,
		cache: &Cache{
			dirEntries: make(map[string][]fuse.Dirent),
			fileData:   make(map[string][]byte),
			ttl:        cfg.CacheTTL,
		},
	}, nil
}

// Cleanup limpia recursos temporales
func (f *FTPFS) Cleanup() {
	if f.tempDir != "" {
		os.RemoveAll(f.tempDir)
		log.Printf("Directorio temporal eliminado: %s", f.tempDir)
	}
	if f.conn != nil {
		f.conn.Quit()
	}
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
	size, err := conn.FileSize(fullPath)
	if err != nil || size < 0 {
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

// ========== IMPLEMENTACIÓN DE ARCHIVO CON CACHE TEMPORAL ==========

// Open implementa fs.NodeOpener
func (a *Archivo) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Si se abre para escritura, crear archivo temporal
	if req.Flags&fuse.OpenWriteOnly != 0 || req.Flags&fuse.OpenReadWrite != 0 {
		// Crear archivo temporal
		tempFile, err := os.CreateTemp(a.fs.tempDir, fmt.Sprintf("temp_*_%s", filepath.Base(a.path)))
		if err != nil {
			return nil, fuse.EIO
		}
		a.tempFile = tempFile.Name()
		tempFile.Close()

		// Descargar archivo existente si existe
		conn, err := a.fs.ObtenerConexion()
		if err != nil {
			return nil, fuse.EIO
		}

		r, err := conn.Retr(a.path)
		if err == nil {
			// Archivo existe, copiarlo al temporal
			f, err := os.OpenFile(a.tempFile, os.O_WRONLY, 0644)
			if err != nil {
				return nil, fuse.EIO
			}
			io.Copy(f, r)
			f.Close()
			r.Close()
		}
		// Si no existe, el archivo temporal queda vacío

		a.isOpen = true
		resp.Flags |= fuse.OpenDirectIO
	}

	return a, nil
}

// Attr retorna los atributos del archivo
func (a *Archivo) Attr(ctx context.Context, attr *fuse.Attr) error {
	a.mu.RLock()
	defer a.mu.RUnlock()

	// Si hay archivo temporal abierto, usar su tamaño
	if a.isOpen && a.tempFile != "" {
		if info, err := os.Stat(a.tempFile); err == nil {
			attr.Mode = 0644
			attr.Size = uint64(info.Size())
			attr.Uid = uint32(os.Getuid())
			attr.Gid = uint32(os.Getgid())
			attr.Valid = 10 * time.Second
			return nil
		}
	}

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

	// Si hay archivo temporal, leer desde allí
	if a.isOpen && a.tempFile != "" {
		f, err := os.Open(a.tempFile)
		if err != nil {
			return fuse.EIO
		}
		defer f.Close()

		buf := make([]byte, req.Size)
		n, err := f.ReadAt(buf, req.Offset)
		if err != nil && err != io.EOF {
			return fuse.EIO
		}
		resp.Data = buf[:n]
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

// Write escribe datos en el archivo temporal
func (a *Archivo) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Si no hay archivo temporal, crearlo
	if a.tempFile == "" {
		tempFile, err := os.CreateTemp(a.fs.tempDir, fmt.Sprintf("temp_direct_*_%s", filepath.Base(a.path)))
		if err != nil {
			return fuse.EIO
		}
		a.tempFile = tempFile.Name()
		tempFile.Close()

		// Intentar descargar archivo existente
		conn, err := a.fs.ObtenerConexion()
		if err == nil {
			r, err := conn.Retr(a.path)
			if err == nil {
				f, _ := os.OpenFile(a.tempFile, os.O_WRONLY, 0644)
				if f != nil {
					io.Copy(f, r)
					f.Close()
				}
				r.Close()
			}
		}
		a.isOpen = true
	}

	// Escribir en archivo temporal
	f, err := os.OpenFile(a.tempFile, os.O_WRONLY, 0644)
	if err != nil {
		return fuse.EIO
	}
	defer f.Close()

	n, err := f.WriteAt(req.Data, req.Offset)
	if err != nil {
		return fuse.EIO
	}

	a.isDirty = true
	resp.Size = n
	return nil
}

// Flush sincroniza los datos - NO sube aún
func (a *Archivo) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	// En Python, flush no hace nada inmediatamente
	// La sincronización real ocurre en release/fsync
	return nil
}

// Fsync sincronización forzada - sube a FTP
func (a *Archivo) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	return a.uploadTempFile()
}

// Release llamado cuando se cierra el archivo - SUBIR a FTP
func (a *Archivo) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Subir archivo si es necesario
	if a.isDirty && a.tempFile != "" {
		if err := a.uploadTempFile(); err != nil {
			log.Printf("Error subiendo archivo en release: %v", err)
		}
	}

	// Limpiar archivo temporal
	if a.tempFile != "" {
		os.Remove(a.tempFile)
		a.tempFile = ""
	}

	a.isOpen = false
	a.isDirty = false

	return nil
}

// uploadTempFile sube el archivo temporal al servidor FTP
func (a *Archivo) uploadTempFile() error {
	if a.tempFile == "" {
		return nil
	}

	if _, err := os.Stat(a.tempFile); os.IsNotExist(err) {
		return nil
	}

	conn, err := a.fs.ObtenerConexion()
	if err != nil {
		return fuse.EIO
	}

	f, err := os.Open(a.tempFile)
	if err != nil {
		return fuse.EIO
	}
	defer f.Close()

	err = conn.Stor(a.path, f)
	if err != nil {
		log.Printf("Error subiendo %s: %v", a.path, err)
		return fuse.EIO
	}

	// Invalidar caché del directorio
	a.fs.cache.mu.Lock()
	delete(a.fs.cache.dirEntries, filepath.Dir(a.path))
	delete(a.fs.cache.fileData, a.path)
	a.fs.cache.mu.Unlock()

	log.Printf("Archivo subido exitosamente: %s", a.path)
	a.isDirty = false

	return nil
}

// Setattr establece atributos del archivo
func (a *Archivo) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	if req.Valid.Size() {
		a.mu.Lock()
		defer a.mu.Unlock()

		// Si hay archivo temporal, truncarlo
		if a.tempFile != "" {
			if err := os.Truncate(a.tempFile, int64(req.Size)); err != nil {
				return fuse.EIO
			}
			a.isDirty = true
		}
	}
	return nil
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
		}
	}

	// Verificar punto de montaje
	if _, err := os.Stat(mountpoint); os.IsNotExist(err) {
		log.Fatalf("El punto de montaje %s no existe", mountpoint)
	}

	fmt.Printf("Montando %s:%d en %s\n", config.Host, config.Port, mountpoint)
	fmt.Printf("Usuario: %s\n", config.User)
	fmt.Println("Para desmontar: fusermount -u", mountpoint)
	fmt.Println("VS Code support version")

	// Crear sistema de archivos FTP
	ftpFS, err := NuevoFTPFS(config)
	if err != nil {
		log.Fatalf("Error creando FTPFS: %v", err)
	}
	defer ftpFS.Cleanup()

	// Configurar FUSE
	fuseConfig := []fuse.MountOption{
		fuse.FSName("ftpfs"),
		fuse.Subtype("ftpfs"),
		fuse.WritebackCache(), // Mejora rendimiento de escritura
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
	fmt.Println("Sistema de archivos montado correctamente. Presiona Ctrl+C para desmontar.")

	err = fs.Serve(c, ftpFS)
	if err != nil {
		log.Fatalf("Error sirviendo FUSE: %v", err)
	}

	fmt.Println("Sistema de archivos desmontado.")
}
