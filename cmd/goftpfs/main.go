package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"

	"github.com/tuusuario/GoFtpfs/internal/ftpfs"
)

func main() {
	// Flags de conexión FTP
	ftpURL := flag.String("ftp-url", "", "FTP URL en formato ftp://[user[:password]@]host[:port][/path]")
	user := flag.String("user", "", "Usuario FTP (sobrescribe URL)")
	pass := flag.String("password", "", "Contraseña FTP (sobrescribe URL)")
	port := flag.Int("port", 0, "Puerto FTP (sobrescribe URL)")
	useTLS := flag.Bool("tls", false, "Usar TLS/SSL (FTPS)")

	// Flags de montaje
	mountpoint := flag.String("mount", "", "Punto de montaje FUSE")
	readOnly := flag.Bool("read-only", false, "Montar solo lectura")
	debug := flag.Bool("debug", false, "Modo debug")

	// Modo simplificado (no FTP) para pruebas
	simple := flag.Bool("simple", false, "Modo simplificado: montar FS mínimo sin FTP (para pruebas)")
	allowOther := flag.Bool("allow-other", false, "Permitir acceso a otros usuarios")
	uid := flag.Int("uid", -1, "UID del propietario")
	gid := flag.Int("gid", -1, "GID del grupo")

	// Flag de ayuda personalizado
	help := flag.Bool("help", false, "Mostrar ayuda")
	flag.BoolVar(help, "h", false, "Mostrar ayuda")

	flag.Parse()

	if *help {
		showHelp()
		return
	}

	// Validar argumentos posicionales si no se proporcionaron flags
	args := flag.Args()

	// Variables para URL parseada (declaradas aquí para uso condicional)
	var server, urlUser, urlPass, path string
	var urlPort int
	var err error
	if *simple {
		// En modo simplificado solo se necesita el punto de montaje
		if *mountpoint == "" && len(args) >= 1 {
			*mountpoint = args[0]
		}
		if *mountpoint == "" {
			fmt.Fprintf(os.Stderr, "Error: Se requiere punto de montaje en modo --simple\n\n")
			showHelp()
			os.Exit(1)
		}
	} else {
		if *ftpURL == "" && len(args) >= 1 {
			*ftpURL = args[0]
		}
		if *mountpoint == "" && len(args) >= 2 {
			*mountpoint = args[1]
		}

		if *ftpURL == "" || *mountpoint == "" {
			fmt.Fprintf(os.Stderr, "Error: Se requiere URL FTP y punto de montaje\n\n")
			showHelp()
			os.Exit(1)
		}

		// Parsear URL FTP
		server, urlUser, urlPass, urlPort, path, err = parseFTPURL(*ftpURL)
		if err != nil {
			log.Fatalf("Error al parsear URL FTP: %v", err)
		}
	}

	// Sobrescribir con flags de línea de comandos
	// El flag --user tiene prioridad sobre la URL
	finalUser := *user
	if finalUser == "" {
		finalUser = urlUser
	}

	finalPass := *pass
	if finalPass == "" {
		finalPass = urlPass
	}

	finalPort := *port
	if finalPort == 0 && urlPort != 0 {
		finalPort = urlPort
	}
	if finalPort == 0 {
		finalPort = 21
	}

	if !*simple {
		if finalUser == "" {
			log.Fatalf("Error: Se requiere nombre de usuario. Use --user o inclúyalo en la URL")
		}
	}

	if *simple {
		server = "local"
	}

	log.Printf("Conectando a servidor FTP: %s:%d", server, finalPort)
	if !*simple {
		log.Printf("Usuario: %s", finalUser)
		log.Printf("TLS: %v", *useTLS)
		log.Printf("Path: %s", path)
	}

	var ftpConn *ftpfs.SimpleFTPConnection
	if !*simple {
		// Crear conexión FTP (simple mode without pooling)
		ftpConn, err = ftpfs.NewSimpleFTPConnection(server, finalUser, finalPass, *useTLS, finalPort, *debug)
		if err != nil {
			log.Fatalf("Error al conectar a FTP: %v", err)
		}

		// Cambiar al directorio inicial si se especificó
		if path != "" && path != "/" {
			if err := ftpConn.Cwd(path); err != nil {
				log.Printf("Advertencia: No se pudo cambiar al directorio %s: %v", path, err)
			}
		}

		// PROBAR CONECTIVIDAD: Listar directorio root antes de montar
		// Esto detecta problemas de conectividad antes de que FUSE esté montado
		log.Printf("Probando listado de directorio root...")
		testCtx, testCancel := context.WithTimeout(context.Background(), 10*time.Second)
		go func() {
			files, err := ftpConn.ListDir("/")
			if err != nil {
				log.Printf("ADVERTENCIA: No se pudo listar directorio root: %v", err)
			} else {
				log.Printf("✓ Conectividad FTP verificada: %d archivos en root", len(files))
			}
			testCancel()
		}()
		<-testCtx.Done()
		if testCtx.Err() == context.DeadlineExceeded {
			log.Printf("ERROR: Timeout al listar directorio root. El servidor FTP no responde.")
			log.Printf("Verifique: 1) Conexión de red, 2) Servidor FTP accesible, 3) Firewall")
			ftpConn.Close()
			os.Exit(1)
		}
	}

	// Configurar punto de montaje
	if err := os.MkdirAll(*mountpoint, 0755); err != nil {
		log.Fatalf("Error al crear punto de montaje: %v", err)
	}

	// Crear filesystem (modo simplificado para pruebas)
	var filesystem fs.FS
	if *simple {
		filesystem = ftpfs.NewSimpleFs(*debug)
	} else {
		filesystem = ftpfs.NewFtpFs(ftpConn, *debug)
	}

	// Precargar caché del directorio root ANTES de montar
	// Esto evita que FUSE se bloquee esperando al FTP
	if !*simple {
		log.Printf("Precargando caché del directorio root...")
		ffs := filesystem.(*ftpfs.FtpFs)
		// Precargar de forma sincrónica para asegurar que está lista antes de montar
		files, err := ftpConn.ListDir("/")
		if err == nil {
			ffs.PreloadDirCache("/", files)
			log.Printf("✓ Caché precargada con %d archivos", len(files))
		} else {
			log.Printf("Advertencia: No se pudo precargar caché: %v", err)
		}
		log.Printf("Continuando después de precarga...")
	}

	log.Printf("Creando punto de montaje...")
	// TEST: Verificar que el filesystem funciona antes de montar
	log.Printf("Verificando filesystem...")
	root, err := filesystem.Root()
	if err != nil {
		log.Fatalf("Error obteniendo root: %v", err)
	}

	// Probar Attr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var attr fuse.Attr
	err = root.Attr(ctx, &attr)
	if err != nil {
		log.Fatalf("Error obteniendo atributos del root: %v", err)
	}
	log.Printf("✓ Filesystem verificado: root inode=%d, mode=%v", attr.Inode, attr.Mode)

	// Configurar UID/GID si se especificaron (solo aplicable a FtpFs)
	if *uid >= 0 {
		if ffs, ok := filesystem.(*ftpfs.FtpFs); ok {
			ffs.SetUID(uint32(*uid))
		}
	}
	if *gid >= 0 {
		if ffs, ok := filesystem.(*ftpfs.FtpFs); ok {
			ffs.SetGID(uint32(*gid))
		}
	}

	// Configurar opciones de montaje
	options := []fuse.MountOption{
		fuse.FSName(fmt.Sprintf("goftpfs@%s:%d", server, finalPort)),
		fuse.Subtype("goftpfs"),
	}

	if *readOnly {
		options = append(options, fuse.ReadOnly())
	}

	if *allowOther {
		options = append(options, fuse.AllowOther())
	}

	// Habilitar debug de FUSE a nivel kernel si estamos en modo debug
	if *debug {
		fuse.Debug = func(msg interface{}) {
			log.Printf("FUSE-KERNEL: %v", msg)
		}
	}

	log.Printf("Montando filesystem FTP en %s...", *mountpoint)

	// Montar filesystem
	conn, err := fuse.Mount(*mountpoint, options...)
	if err != nil {
		log.Fatalf("Error al montar: %v", err)
	}
	defer conn.Close()

	// El mount está listo, comenzar a servir inmediatamente
	log.Printf("✓ Filesystem montado en %s", *mountpoint)

	// Canal para señalizar cuando fs.Serve termina
	serveDone := make(chan error, 1)

	// Servir filesystem en goroutine
	go func() {
		log.Printf("Servidor FUSE iniciado. Esperando peticiones...")
		log.Printf("Pruebe acceder con: ls -la %s", *mountpoint)
		if err := fs.Serve(conn, filesystem); err != nil {
			serveDone <- err
		} else {
			serveDone <- nil
		}
	}()

	// Manejar señales para desmontar limpiamente
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigs:
		log.Printf("Recibida señal %v, desmontando %s...", sig, *mountpoint)
	case err := <-serveDone:
		if err != nil {
			log.Printf("Error en servidor FUSE: %v", err)
		}
		log.Printf("Servidor FUSE terminado, desmontando %s...", *mountpoint)
	}

	// Intentar desmontar
	if err := fuse.Unmount(*mountpoint); err != nil {
		log.Printf("Advertencia: error al desmontar con fuse.Unmount: %v", err)
		// Intentar con fusermount como fallback
		if err := exec.Command("fusermount", "-u", *mountpoint).Run(); err != nil {
			log.Printf("Advertencia: error al desmontar con fusermount: %v", err)
		}
	}
	log.Printf("Desmontaje completado")
}

func showHelp() {
	fmt.Println(`Uso: goftpfs [OPCIONES] <URL_FTP> <PUNTO_MONTAJE>

IMPORTANTE: Las opciones (flags) deben ir ANTES de los argumentos posicionales.

Argumentos posicionales:
  URL_FTP       URL FTP en formato ftp://[user[:password]@]host[:port][/path]
  PUNTO_MONTAJE Directorio local para montar el filesystem FTP

Opciones de conexión:
  -ftp-url string    URL FTP (alternativa a argumento posicional)
  -user string       Usuario FTP (sobrescribe URL)
  -password string   Contraseña FTP (sobrescribe URL)
  -port int          Puerto FTP (default: 21)
  -tls               Usar TLS/SSL (FTPS)

Opciones de montaje:
  -mount string      Punto de montaje (alternativa a argumento posicional)
  -read-only         Montar solo lectura
  -debug             Modo debug
  -allow-other       Permitir acceso a otros usuarios
  -uid int           UID del propietario
  -gid int           GID del grupo

Ejemplos correctos:
  goftpfs ftp://user:pass@ftp.example.com /mnt/ftp
  goftpfs --user myuser --password mypass ftp://ftp.example.com /mnt/ftp
  goftpfs -u myuser -p mypass --tls ftp://ftp.example.com:2121 /mnt/ftp
  goftpfs --debug --user myuser ftp://ftp.example.com /mnt/ftp

Ejemplos incorrectos (los flags no se reconocerán):
  goftpfs ftp://ftp.example.com /mnt/ftp --user myuser  # INCORRECTO
`)
}

func parseFTPURL(urlStr string) (server, user, pass string, port int, path string, err error) {
	// Asegurar que tiene protocolo
	if !strings.Contains(urlStr, "://") {
		urlStr = "ftp://" + urlStr
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return "", "", "", 0, "", fmt.Errorf("error al parsear URL: %v", err)
	}

	// Validar esquema
	if u.Scheme != "ftp" && u.Scheme != "ftps" {
		return "", "", "", 0, "", fmt.Errorf("el esquema debe ser 'ftp://' o 'ftps://'")
	}

	// Extraer host
	if u.Host == "" {
		return "", "", "", 0, "", fmt.Errorf("la URL debe contener un host")
	}

	// Extraer puerto
	host := u.Hostname()
	portStr := u.Port()
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return "", "", "", 0, "", fmt.Errorf("puerto inválido: %v", err)
		}
		port = p
	}

	// Extraer credenciales
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}

	// Extraer path (sin slash inicial)
	path = u.Path
	if path == "" {
		path = "/"
	}

	return host, user, pass, port, path, nil
}
