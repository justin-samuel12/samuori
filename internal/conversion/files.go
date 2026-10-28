package conversion

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Snapshot copies the tree at dir into into, keeping modes and, as root, owners, so Restore can put the host's
// configuration back exactly as it was.
func Snapshot(dir, into string) error {
	if err := os.MkdirAll(filepath.Dir(into), 0o700); err != nil {
		return err
	}
	return copyTree(dir, into)
}

// Restore replaces the tree at dir with the snapshot taken into from.
func Restore(from, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return copyTree(from, dir)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, raw, info.Mode().Perm()); err != nil {
				return err
			}
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
		default:
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
			return os.Lchown(target, int(st.Uid), int(st.Gid))
		}
		return nil
	})
}
