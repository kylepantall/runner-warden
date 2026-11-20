//go:build unix

package migrate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// All filesystem work on untrusted trees goes through directory fds opened
// with O_NOFOLLOW|O_DIRECTORY one component at a time, so a symlink anywhere
// in the path (or swapped in later) can never redirect an operation.

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

var errUnsafePath = errors.New("unsafe path")

// dirInfo identifies a directory for later re-validation.
type dirInfo struct {
	Dev, Ino uint64
	UID      uint32
}

func plainName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00")
}

// splitLogical splits an absolute, clean logical path into components.
func splitLogical(logical string) ([]string, error) {
	if !strings.HasPrefix(logical, "/") || strings.ContainsRune(logical, 0) {
		return nil, errUnsafePath
	}
	var out []string
	for _, c := range strings.Split(logical, "/") {
		if c == "" {
			continue
		}
		if !plainName(c) {
			return nil, errUnsafePath
		}
		out = append(out, c)
	}
	return out, nil
}

// openPlainDir opens a trusted directory path (the filesystem root, or the
// state dir), following symlinks.
func openPlainDir(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// openChain opens root, then each component of logical with no-follow.
func openChain(root, logical string) (int, error) {
	comps, err := splitLogical(logical)
	if err != nil {
		return -1, err
	}
	fd, err := openPlainDir(root)
	if err != nil {
		return -1, err
	}
	for _, c := range comps {
		next, err := unix.Openat(fd, c, dirFlags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}

func closeFd(fd int) { _ = unix.Close(fd) }

func fstatDir(fd int) (dirInfo, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return dirInfo{}, err
	}
	return dirInfo{Dev: uint64(st.Dev), Ino: st.Ino, UID: st.Uid}, nil //nolint:gosec,unconvert // st.Dev is int32 on darwin and an opaque id; never negative
}

// statChain returns the identity of the directory at logical, refusing any
// symlink on the way.
func statChain(root, logical string) (dirInfo, error) {
	fd, err := openChain(root, logical)
	if err != nil {
		return dirInfo{}, err
	}
	defer closeFd(fd)
	return fstatDir(fd)
}

// statRegularAt reports the owner of the regular file name in dirfd (no-follow).
func statRegularAt(dirfd int, name string) (uid uint32, err error) {
	if !plainName(name) {
		return 0, errUnsafePath
	}
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG { //nolint:unconvert // st.Mode is uint16 on darwin, uint32 on linux
		return 0, errUnsafePath
	}
	return st.Uid, nil
}

// readFileAt reads a regular file in dirfd with no-follow, capped at limit bytes.
func readFileAt(dirfd int, name string, limit int64) ([]byte, error) {
	if !plainName(name) {
		return nil, errUnsafePath
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

func existsAt(dirfd int, name string) (bool, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.ENOENT):
		return false, nil
	}
	return false, err
}

func listDirAt(dirfd int) ([]string, error) {
	fd, err := unix.Dup(dirfd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "dir")
	defer f.Close()
	return f.Readdirnames(-1)
}

// renameAt moves name from srcfd to dstfd without following symlinks and
// without overwriting. Across filesystems it copies (no-follow) then removes.
func renameAt(srcfd int, src string, dstfd int, dst string) error {
	if !plainName(src) || !plainName(dst) {
		return errUnsafePath
	}
	if ok, err := existsAt(srcfd, src); err != nil {
		return err
	} else if !ok {
		return fs.ErrNotExist
	}
	if ok, err := existsAt(dstfd, dst); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("destination exists: %w", fs.ErrExist)
	}
	err := unix.Renameat(srcfd, src, dstfd, dst)
	if !errors.Is(err, unix.EXDEV) {
		return err
	}
	if err := copyAt(srcfd, src, dstfd, dst); err != nil {
		_ = removeAllAt(dstfd, dst)
		return err
	}
	return removeAllAt(srcfd, src)
}

func copyAt(srcfd int, src string, dstfd int, dst string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(srcfd, src, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	perm := uint32(st.Mode) & 0o7777       //nolint:unconvert // st.Mode is uint16 on darwin, uint32 on linux
	switch uint32(st.Mode) & unix.S_IFMT { //nolint:unconvert // st.Mode is uint16 on darwin, uint32 on linux
	case unix.S_IFDIR:
		if err := unix.Mkdirat(dstfd, dst, perm|0o700); err != nil {
			return err
		}
		sfd, err := unix.Openat(srcfd, src, dirFlags, 0)
		if err != nil {
			return err
		}
		defer closeFd(sfd)
		dfd, err := unix.Openat(dstfd, dst, dirFlags, 0)
		if err != nil {
			return err
		}
		defer closeFd(dfd)
		names, err := listDirAt(sfd)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := copyAt(sfd, n, dfd, n); err != nil {
				return err
			}
		}
		_ = unix.Fchmod(dfd, perm)
	case unix.S_IFLNK:
		buf := make([]byte, 4096)
		n, err := unix.Readlinkat(srcfd, src, buf)
		if err != nil {
			return err
		}
		if err := unix.Symlinkat(string(buf[:n]), dstfd, dst); err != nil {
			return err
		}
	case unix.S_IFREG:
		in, err := unix.Openat(srcfd, src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		inf := os.NewFile(uintptr(in), src)
		defer inf.Close()
		out, err := unix.Openat(dstfd, dst, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
		if err != nil {
			return err
		}
		outf := os.NewFile(uintptr(out), dst)
		if _, err := io.Copy(outf, inf); err != nil {
			outf.Close()
			return err
		}
		_ = unix.Fchmod(out, perm)
		if err := outf.Close(); err != nil {
			return err
		}
	default:
		return nil
	}
	_ = unix.Fchownat(dstfd, dst, int(st.Uid), int(st.Gid), unix.AT_SYMLINK_NOFOLLOW)
	return nil
}

func removeAllAt(dirfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR { //nolint:unconvert // st.Mode is uint16 on darwin, uint32 on linux
		return unix.Unlinkat(dirfd, name, 0)
	}
	fd, err := unix.Openat(dirfd, name, dirFlags, 0)
	if err != nil {
		return err
	}
	names, err := listDirAt(fd)
	if err != nil {
		closeFd(fd)
		return err
	}
	for _, n := range names {
		if err := removeAllAt(fd, n); err != nil {
			closeFd(fd)
			return err
		}
	}
	closeFd(fd)
	return unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR)
}
