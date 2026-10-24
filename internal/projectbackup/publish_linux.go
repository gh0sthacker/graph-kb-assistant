//go:build linux

package projectbackup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func publishDirectoryNoReplace(source, destination string) error {
	sourceDir, err := unix.Open(filepath.Dir(source), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sourceDir)
	destinationDir, err := unix.Open(filepath.Dir(destination), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(destinationDir)
	return unix.Renameat2(sourceDir, filepath.Base(source), destinationDir, filepath.Base(destination), unix.RENAME_NOREPLACE)
}

func openProjectFile(root, relative string) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	return openRelativeRegular(rootFD, strings.Split(relative, "/"))
}

func openRegularNoFollow(filename string) (*os.File, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD)
	parts := strings.Split(strings.TrimPrefix(absolute, string(filepath.Separator)), string(filepath.Separator))
	return openRelativeRegular(rootFD, parts)
}

func openRelativeRegular(rootFD int, components []string) (*os.File, error) {
	if len(components) == 0 {
		return nil, errors.New("file path is empty")
	}
	currentFD := rootFD
	ownedFD := false
	for i, component := range components {
		if component == "" || component == "." || component == ".." {
			if ownedFD {
				_ = unix.Close(currentFD)
			}
			return nil, errors.New("file path contains an unsafe component")
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		openedFD, err := unix.Openat(currentFD, component, flags, 0)
		if ownedFD {
			_ = unix.Close(currentFD)
		}
		if err != nil {
			return nil, err
		}
		if i == len(components)-1 {
			var info unix.Stat_t
			if err := unix.Fstat(openedFD, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG {
				_ = unix.Close(openedFD)
				return nil, errors.New("path does not resolve to a regular file")
			}
			return os.NewFile(uintptr(openedFD), component), nil
		}
		currentFD = openedFD
		ownedFD = true
	}
	return nil, errors.New("file path could not be opened")
}
