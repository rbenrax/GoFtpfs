Vendoring and building the GoFtpfs executable

Overview
- The project is prepared to build an executable that exposes an FTP server via a FUSE filesystem in Go. To ensure reproducible builds in environments with restricted network access, the dependencies can be vendored and built offline.

What you need
- A machine with internet access to vendor dependencies at least once, and a Linux environment with FUSE support.
- Go 1.20+ (in this repo we use 1.22+ in tests).

Steps to produce a self-contained executable
- Run: chmod +x build_vendor.sh
- Run: ./build_vendor.sh
- After completion, the binary will be at ./bin/goftpfs
- Run the binary with arguments, for example:
  ./bin/goftpfs -mount /mnt/goftpfsh -addr ftp.example.com:21 -user user -pass pass -root /

Notes
- The vendor directory is created by the script and should be committed if you want fully offline reproducibility.
- If you need to adjust root or other defaults, modify main.go or supply flags at runtime.
