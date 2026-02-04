# Corrección Importante: Eliminación de Deadlocks en GoFtpfs

## Problema Principal Identificado

**DEADLOCK en `getOrCreateInode()`** - Esta función causaba bloqueos permanentes del sistema FUSE.

### Causa del Deadlock

```go
// ANTES (Causaba deadlock):
func (f *FtpFs) getOrCreateInode(parent uint64, fileInfo *FtpFileInfo) *Inode {
    f.inodeMu.Lock()           // ← Lock adquirido aquí
    defer f.inodeMu.Unlock()   // ← Liberado al final
    
    // ... código ...
    
    ino := f.allocateInode()   // ← INTENTA adquirir el MISMO lock → DEADLOCK!
    
    // ... más código ...
}

func (f *FtpFs) allocateInode() uint64 {
    f.inodeMu.Lock()           // ← Bloqueo infinito, lock ya está tomado
    defer f.inodeMu.Unlock()
    // ...
}
```

**Cuándo ocurría:**
- Al precargar el caché con `PreloadDirCache()`
- Al crear inodos durante `ReadDirAll()`
- Cualquier operación que creara nuevos inodos

### Solución Implementada

```go
// DESPUÉS (Sin deadlock):
func (f *FtpFs) getOrCreateInode(parent uint64, fileInfo *FtpFileInfo) *Inode {
    f.inodeMu.Lock()
    
    // Check if inode already exists
    if ino, ok := f.pathToInode[fileInfo.Path]; ok {
        if inode, ok := f.inodes[ino]; ok {
            f.inodeMu.Unlock()      // ← Liberar ANTES de retornar
            return inode
        }
    }
    f.inodeMu.Unlock()              // ← Liberar ANTES de allocateInode
    
    // Allocate new inode number (needs separate lock)
    ino := f.allocateInode()        // ← Ahora puede adquirir el lock
    
    // ... crear inode ...
    
    // Need lock for maps
    f.inodeMu.Lock()                // ← Readquirir para modificar maps
    f.inodes[ino] = inode
    f.pathToInode[fileInfo.Path] = ino
    f.inodeMu.Unlock()              // ← Liberar inmediatamente después
    
    return inode
}
```

**Cambios clave:**
1. Liberar `inodeMu` antes de llamar a `allocateInode()`
2. Readquirir el lock solo cuando sea necesario modificar los maps
3. Minimizar el tiempo de hold del lock

## Mejoras Adicionales

### 1. Precarga de Caché
- El directorio root se carga **antes** de montar FUSE
- Evita que el primer acceso bloquee el sistema

### 2. Timeouts en Operaciones FTP
- `ListDir`: 15 segundos
- `ReadDirAll`: 20 segundos  
- `Lookup`: 500ms (fail-fast)

### 3. Caché con Stale-Data
- Si el caché expira, se devuelven datos antiguos
- La actualización ocurre en background
- Nunca se bloquea al usuario

### 4. Fast-Fail en Lookup
- Si el caché no está listo, retorna ENOENT inmediatamente
- El caché se poblará en el próximo acceso

## Resultado

**Antes:** El sistema se bloqueaba al acceder al volumen montado
**Después:** Respuesta en menos de 1ms, sistema completamente funcional

```bash
$ time ls /home/rafa/2150/
bin boot.py etc lib libx LICENSE local main.py opt README.md tmp t.sh var www

real    0m0.001s
user    0m0.000s
sys     0m0.001s
```

## Archivos Modificados

1. `/internal/ftpfs/filesystem.go` - Corrección del deadlock
2. `/internal/ftpfs/ftp.go` - Timeouts en operaciones FTP
3. `/cmd/goftpfs/main.go` - Precarga de caché

## Testing

Comando de prueba utilizado:
```bash
./goftpfs ftp://admin:rborbo@192.168.2.150 /home/rafa/2150/
```

Verificación:
- ✅ Montaje exitoso
- ✅ Listado de archivos (< 1ms)
- ✅ Navegación de directorios
- ✅ Sin bloqueos
- ✅ Desmontaje limpio
