//go:build !linux

package projectbackup

import (
	"errors"
	"os"
	"path/filepath"
)

func publishDirectoryNoReplace(_, _ string) error {
	return errors.New("project restore publication requires Linux no-replace rename support")
}

func openProjectFile(root, relative string) (*os.File, error) {
	return openRegularNoFollow(filepath.Join(root, filepath.FromSlash(relative)))
}

func openRegularNoFollow(filename string) (*os.File, error) {
	info, err := os.Lstat(filename)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return os.Open(filename)
}
