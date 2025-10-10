# GoFtpfs 
Go version of fuse ftpfs utlity, (Not ready yet, working on)

#  Install dependencies
go mod tidy

# Compile
go build -o ftpfs ftpfs.go

# Mount FTP
./ftpfs ftp.server.com /mnt/ftp -u user -P password

# Mount with específic options
./ftpfs ftp.server.com /mnt/ftp -p 21 -u user -P password -d

# Umount
fusermount -u /mnt/ftp
