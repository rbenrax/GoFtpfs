#!/bin/bash
# Script de diagnóstico para GoFtpfs

set -e

MOUNTPOINT="/tmp/goftpfs-test"
FTP_SERVER="${FTP_SERVER:-ftp.example.com}"
FTP_USER="${FTP_USER:-anonymous}"
FTP_PASS="${FTP_PASS:-anonymous@}"

echo "=== Diagnóstico de GoFtpfs ==="
echo ""

# Verificar que FUSE está instalado
echo "1. Verificando FUSE..."
if ! command -v fusermount &> /dev/null; then
    echo "   ERROR: fusermount no está instalado"
    echo "   Instale fuse: sudo apt-get install fuse"
    exit 1
fi
echo "   ✓ FUSE instalado"

# Verificar permisos
echo ""
echo "2. Verificando permisos..."
if [ -r /dev/fuse ]; then
    echo "   ✓ Acceso a /dev/fuse"
else
    echo "   ERROR: No hay acceso a /dev/fuse"
    echo "   Ejecute: sudo usermod -aG fuse $USER"
    exit 1
fi

# Limpiar montajes previos
echo ""
echo "3. Limpiando montajes previos..."
if mountpoint -q "$MOUNTPOINT" 2>/dev/null; then
    echo "   Desmontando $MOUNTPOINT..."
    fusermount -u "$MOUNTPOINT" 2>/dev/null || true
    sleep 1
fi

# Crear punto de montaje
rm -rf "$MOUNTPOINT"
mkdir -p "$MOUNTPOINT"
echo "   ✓ Punto de montaje creado: $MOUNTPOINT"

# Probar modo simple primero (sin FTP)
echo ""
echo "4. Probando modo simple (sin FTP)..."
./goftpfs --simple --debug "$MOUNTPOINT" &
PID=$!
sleep 3

# Verificar si está montado
if mountpoint -q "$MOUNTPOINT"; then
    echo "   ✓ Sistema montado correctamente"
    echo ""
    echo "5. Probando acceso..."
    ls -la "$MOUNTPOINT" && echo "   ✓ Acceso funciona"
else
    echo "   ERROR: El sistema no aparece montado"
fi

# Detener
kill $PID 2>/dev/null || true
wait $PID 2>/dev/null || true
fusermount -u "$MOUNTPOINT" 2>/dev/null || true
sleep 1

echo ""
echo "6. Probando conexión FTP..."
echo "   Servidor: $FTP_SERVER"
echo "   Usuario: $FTP_USER"

# Verificar si el servidor FTP es accesible
timeout 5 bash -c "echo > /dev/tcp/${FTP_SERVER%%:*}/21" 2>/dev/null && echo "   ✓ Servidor FTP accesible" || echo "   ⚠ No se pudo conectar al puerto 21"

echo ""
echo "7. Probando montaje con FTP..."
./goftpfs --debug "ftp://${FTP_USER}:${FTP_PASS}@${FTP_SERVER}" "$MOUNTPOINT" &
PID=$!

# Esperar hasta 15 segundos
for i in {1..15}; do
    if mountpoint -q "$MOUNTPOINT"; then
        echo "   ✓ Sistema montado con FTP"
        echo ""
        echo "8. Probando listado..."
        timeout 5 ls -la "$MOUNTPOINT" && echo "   ✓ Listado funciona" || echo "   ⚠ Listado falló o timeout"
        break
    fi
    sleep 1
done

if ! mountpoint -q "$MOUNTPOINT"; then
    echo "   ERROR: Timeout esperando montaje"
fi

# Limpieza
echo ""
echo "9. Limpiando..."
kill $PID 2>/dev/null || true
wait $PID 2>/dev/null || true
fusermount -u "$MOUNTPOINT" 2>/dev/null || true
rm -rf "$MOUNTPOINT"

echo ""
echo "=== Diagnóstico completado ==="
