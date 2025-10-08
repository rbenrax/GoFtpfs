# GoFtpfs
Go version of fuse ftpfs utlity
# Instalar dependencias
go mod tidy

# Compilar
go build -o ftpfs ftpfs.go

# Montar FTP
./ftpfs ftp.miservidor.com /mnt/ftp -u usuario -P contraseña

# Montar con opciones específicas
./ftpfs ftp.ejemplo.com /mnt/ftp -p 2121 -u mi_usuario -P mi_contraseña -d

# Desmontar
fusermount -u /mnt/ftp
