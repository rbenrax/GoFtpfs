# GoFtpfs (Not ready yet, working on)
Go version of fuse ftpfs utlity

#  Install dependencies
go mod tidy

# Compile
go build -o ftpfs ftpfs.go

# Mount FTP
./ftpfs ftp.miservidor.com /mnt/ftp -u user -P password

# Monut with específic options
./ftpfs ftp.ejemplo.com /mnt/ftp -p 2121 -u user -P password -d

# Umount
fusermount -u /mnt/ftp
