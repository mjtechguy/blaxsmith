package tooladapter

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PrepareVerification runs before any candidate command. Root retains the
// checkout and daemon; checks receive distinct unprivileged UIDs and writable
// homes only. A candidate cannot rewrite the verifier, Git metadata, or a
// subsequent check's environment. This mode intentionally refuses builds that
// require writing into the source tree.
func PrepareVerification(source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrBlocked
	}
	if err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = os.Lchown(path, 0, 0); err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0444)
		if d.IsDir() || info.Mode()&0111 != 0 {
			mode = 0555
		}
		if !d.IsDir() && !info.Mode().IsRegular() {
			return ErrBlocked
		}
		return os.Chmod(path, mode)
	}); err != nil {
		return err
	}
	base := "/tmp/blaxsmith-verification"
	if err := os.Mkdir(base, 0755); err != nil {
		return err
	} // existing state is not fresh
	for i := range 64 {
		home := filepath.Join(base, fmt.Sprint(i))
		if err := os.Mkdir(home, 0700); err != nil {
			return err
		}
		if err := os.Chown(home, 60000+i, 60000+i); err != nil {
			return err
		}
	}
	return nil
}
