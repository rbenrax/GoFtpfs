# GoFtpfs

**GoFtpfs** is a tool written in Go that allows you to mount FTP file systems using FUSE (FTP filesystem), providing a way to work with remote FTP servers as if they were local file systems.  
> ⚠️ *This project is still under development and is not ready for production use.*

---

## 📌 Features

- Mount a remote FTP server as a local file system.
- Written in **Golang** and based on FUSE.
- Allows access to FTP files using local paths once mounted.

---

## 🚀 Requirements

Before building and using GoFtpfs, make sure you have:

- **Go** installed (recommended version ≥ 1.18).
- A system with **FUSE** support (Linux/macOS with fuse installed).
- Proper permissions and configuration for FUSE on your system.

---

## 🛠️ Installation

```bash
git clone https://github.com/rbenrax/GoFtpfs.git
cd GoFtpfs
go mod tidy
go build -o ftpfs ./cmd/goftpfs
```

---

## 🚀 Usage

### 🌐 Mount an FTP server

```bash
./ftpfs ftp.server.com /mnt/ftp -u username -P password
```

### 🔧 Common options

| Option | Description |
|-------|-------------|
| `-p <port>` | FTP port (default: 21) |
| `-u <username>` | FTP username |
| `-P <password>` | FTP password |
| `-d` | Debug mode |

Example with options:

```bash
./ftpfs ftp.server.com /mnt/ftp -p 21 -u myuser -P mypassword -d
```

---

## 🔌 Unmount the filesystem

Once mounted, you can unmount it using:

```bash
fusermount -u /mnt/ftp
```

---

## 🧪 Project status

This project is **not finished yet**; several features and improvements are still under development. Use with caution and check the *issues* section to see what is pending or planned.

---

## 📝 Contributing

If you want to contribute:

1. Fork the repository.
2. Create a new branch for your feature or fix (`git checkout -b feature/name`).
3. Commit your changes (`git commit -m "Description"`).
4. Open a *pull request*.

---

## 📜 License

This project is licensed under the **Apache-2.0 License**.

---

## 📎 Resources

- 📦 Source code: https://github.com/rbenrax/GoFtpfs
