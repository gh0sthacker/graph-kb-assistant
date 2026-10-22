package project

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

var networkFilesystems = []string{
	"cifs", "smb", "nfs", "9p", "sshfs", "fuse.sshfs", "davfs", "davfs2",
	"fuse.davfs", "ceph", "glusterfs", "afs", "lustre", "gpfs", "curlftpfs",
	"fuse.curlftpfs",
}

// DataRoot resolves the private runtime root without creating it. The root is
// deliberately outside the source checkout by convention; ProjectStore also
// enforces that boundary when it opens a project database.
func DataRoot() (string, error) {
	var candidate string
	if value, ok := os.LookupEnv("BB_ASSISTANT_DATA_HOME"); ok {
		candidate = expandHome(value)
	} else if value, ok := os.LookupEnv("XDG_DATA_HOME"); ok {
		candidate = filepath.Join(expandHome(value), "bb-graph-assistant")
	} else {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("could not resolve the private data root")
		}
		candidate = filepath.Join(home, ".local", "share", "bb-graph-assistant")
	}
	resolved, err := resolvePath(candidate)
	if err != nil {
		return "", errors.New("could not resolve the private data root")
	}
	return resolved, nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// EnsurePrivateDirectory creates a directory and verifies that it resolves to
// a local filesystem location with no group/other permissions.
func EnsurePrivateDirectory(path string) (string, error) {
	requested, err := resolvePath(path)
	if err != nil {
		return "", errors.New("could not resolve private runtime directory")
	}
	if err := rejectNetworkFilesystem(requested); err != nil {
		return "", err
	}
	if err := os.MkdirAll(requested, 0o700); err != nil {
		return "", errors.New("could not create private runtime directory")
	}
	resolved, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", errors.New("could not resolve private runtime directory")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("could not resolve private runtime directory")
	}
	if err := rejectNetworkFilesystem(resolved); err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("private runtime path is not a directory")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(resolved, 0o700); err != nil {
			return "", errors.New("could not restrict runtime directory permissions")
		}
		info, err = os.Stat(resolved)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			return "", errors.New("runtime directory permissions are not private")
		}
	}
	return filepath.Clean(resolved), nil
}

func resolvePath(path string) (string, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(filepath.Clean(expandHome(path)))
	if err != nil {
		return "", err
	}
	probe := absolute
	var missing []string
	for {
		_, statErr := os.Lstat(probe)
		if statErr == nil {
			resolved, evalErr := filepath.EvalSymlinks(probe)
			if evalErr != nil {
				return "", evalErr
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", statErr
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

func rejectNetworkFilesystem(path string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	mount, ok := mountForPath(path)
	if !ok {
		return nil
	}
	filesystem := strings.ToLower(mount.filesystem)
	for _, blocked := range networkFilesystems {
		if filesystem == blocked || strings.HasPrefix(filesystem, blocked+".") {
			return errors.New("runtime data must be stored on a local filesystem")
		}
	}
	return nil
}

type mountInfo struct {
	filesystem string
	point      string
}

func mountForPath(path string) (mountInfo, bool) {
	contents, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return mountInfo{}, false
	}
	cleanPath := filepath.Clean(path)
	best := mountInfo{}
	bestDepth := -1
	for _, line := range strings.Split(string(contents), "\n") {
		before, after, found := strings.Cut(line, " - ")
		if !found {
			continue
		}
		left := strings.Fields(before)
		right := strings.Fields(after)
		if len(left) < 5 || len(right) == 0 {
			continue
		}
		point := unescapeMountField(left[4])
		rel, relErr := filepath.Rel(filepath.Clean(point), cleanPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		depth := len(filepath.Clean(point))
		if depth > bestDepth {
			bestDepth = depth
			best = mountInfo{filesystem: right[0], point: filepath.Clean(point)}
		}
	}
	return best, bestDepth >= 0
}

func unescapeMountField(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
