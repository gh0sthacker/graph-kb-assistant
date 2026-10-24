// Package projectbackup creates and restores bounded, private project archives.
// Archive members contain project files only; the global registry and live
// process locks are deliberately reconstructed rather than copied.
package projectbackup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"bb-graph-assistant/internal/project"
	"bb-graph-assistant/internal/projectlock"
	"bb-graph-assistant/internal/store"
)

const (
	archiveFormat   = "bb-graph-assistant-project"
	archiveVersion  = 1
	manifestName    = "manifest.json"
	maxManifestSize = 1 << 20
)

var projectKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Limits are mandatory resource budgets for both archive creation and restore.
// MaxBytes applies to the sum of uncompressed project file sizes.
type Limits struct {
	MaxBytes int64
	MaxFiles int
}

// Report contains aggregate metadata only. It never includes file names,
// project display names, vault contents, or request data.
type Report struct {
	State         string `json:"state"`
	ProjectKey    string `json:"project_key"`
	FileCount     int    `json:"file_count"`
	TotalBytes    int64  `json:"total_bytes"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

type archiveManifest struct {
	Format     string                  `json:"format"`
	Version    int                     `json:"version"`
	Project    project.CaidoProjectRef `json:"project"`
	ProjectKey string                  `json:"project_key"`
	Files      []fileEntry             `json:"files"`
}

type fileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type sourceFile struct {
	path  string
	entry fileEntry
}

// Backup writes one tar archive for the explicitly registered project. It
// takes the project's exclusive maintenance lease while it inventories and
// reads files, so assistant mutations cannot race the snapshot.
func Backup(ctx context.Context, dataRoot, projectKey, archivePath string, limits Limits) (Report, error) {
	if ctx == nil || !validLimits(limits) || !projectKeyPattern.MatchString(projectKey) {
		return Report{}, errors.New("backup requires a project key and explicit positive limits")
	}
	if archivePath == "" {
		return Report{}, errors.New("backup archive path is required")
	}
	refs, err := project.LoadProjectRefsReadOnly(dataRoot)
	if err != nil {
		return Report{}, errors.New("project registry could not be read")
	}
	var selected *project.CaidoProjectRef
	for _, ref := range refs {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return Report{}, errors.New("project registry contains an invalid identity")
		}
		if key == projectKey {
			copy := ref
			selected = &copy
			break
		}
	}
	if selected == nil {
		return Report{}, errors.New("selected project is not registered locally")
	}
	projectDir, err := store.ProjectDirectory(*selected, dataRoot)
	if err != nil {
		return Report{}, errors.New("selected project directory is unavailable")
	}
	archivePath, err = prepareNewFilePath(archivePath)
	if err != nil {
		return Report{}, err
	}
	if pathWithin(projectDir, archivePath) {
		return Report{}, errors.New("backup archive must be outside the selected project directory")
	}
	lease, err := projectlock.Acquire(ctx, projectDir)
	if err != nil {
		return Report{}, errors.New("project maintenance lease could not be acquired")
	}
	defer lease.Close()

	files, total, err := inventory(ctx, projectDir, limits)
	if err != nil {
		return Report{}, err
	}
	manifest := archiveManifest{
		Format: archiveFormat, Version: archiveVersion, Project: *selected,
		ProjectKey: projectKey, Files: make([]fileEntry, len(files)),
	}
	for i := range files {
		manifest.Files[i] = files[i].entry
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil || len(manifestBytes) > maxManifestSize {
		return Report{}, errors.New("project manifest exceeds the supported size")
	}
	if err := writeArchive(ctx, projectDir, archivePath, manifestBytes, files); err != nil {
		return Report{}, err
	}
	digest, err := digestFile(ctx, archivePath, archiveSizeLimit(limits))
	if err != nil {
		if cleanupErr := os.Remove(archivePath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			return Report{}, errors.New("completed backup could not be verified and cleanup did not complete")
		}
		return Report{}, errors.New("completed archive could not be verified")
	}
	return Report{State: "complete", ProjectKey: projectKey, FileCount: len(files), TotalBytes: total, ArchiveSHA256: digest}, nil
}

// Restore validates the entire archive before extracting into a private
// temporary project directory. The project is atomically published and added
// to the local registry only after all members pass their digest checks.
func Restore(ctx context.Context, archivePath, dataRoot string, limits Limits) (report Report, retErr error) {
	if ctx == nil || !validLimits(limits) || archivePath == "" {
		return Report{}, errors.New("restore requires an archive path and explicit positive limits")
	}
	archivePath, err := existingRegularPath(archivePath)
	if err != nil {
		return Report{}, errors.New("restore archive must be a regular file without symlinked path components")
	}
	manifest, digest, total, err := validateArchive(ctx, archivePath, limits)
	if err != nil {
		return Report{}, err
	}
	root, err := store.ValidateDataRoot(dataRoot)
	if err != nil {
		return Report{}, errors.New("restore data root is invalid or points into the source checkout")
	}
	if err := rejectSymlinkComponents(root); err != nil {
		return Report{}, errors.New("restore data root contains a symbolic link")
	}
	registered, err := project.LoadProjectRefsReadOnly(root)
	if err != nil {
		return Report{}, errors.New("existing project registry could not be checked")
	}
	for _, ref := range registered {
		key, keyErr := ref.StorageKey()
		if keyErr != nil {
			return Report{}, errors.New("existing project registry contains an invalid identity")
		}
		if key == manifest.ProjectKey {
			return Report{}, errors.New("refusing to replace an already registered project identity")
		}
	}
	requestedProjectsDir := filepath.Join(root, "projects")
	if err := rejectSymlinkComponents(requestedProjectsDir); err != nil {
		return Report{}, errors.New("projects directory contains a symbolic link")
	}
	projectsDir, err := project.EnsurePrivateDirectory(requestedProjectsDir)
	if err != nil {
		return Report{}, errors.New("private projects directory could not be prepared")
	}
	if err := rejectSymlinkComponents(projectsDir); err != nil {
		return Report{}, errors.New("projects directory contains a symbolic link")
	}
	finalDir := filepath.Join(projectsDir, manifest.ProjectKey)
	if _, err := os.Lstat(finalDir); err == nil {
		return Report{}, errors.New("refusing to overwrite an existing project directory")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Report{}, errors.New("project destination could not be checked")
	}
	temporaryDir, err := os.MkdirTemp(projectsDir, ".restore-*")
	if err != nil {
		return Report{}, errors.New("private restore workspace could not be created")
	}
	if err := os.Chmod(temporaryDir, 0o700); err != nil {
		_ = os.RemoveAll(temporaryDir)
		return Report{}, errors.New("restore workspace permissions could not be restricted")
	}
	published := false
	defer func() {
		if !published {
			if cleanupErr := os.RemoveAll(temporaryDir); cleanupErr != nil {
				retErr = errors.Join(retErr, errors.New("private restore workspace cleanup did not complete"))
			}
		}
	}()
	if err := extractArchive(ctx, archivePath, temporaryDir, manifest); err != nil {
		return Report{}, err
	}
	for _, requiredDirectory := range []string{"vault", "behavior-receipts"} {
		if err := os.MkdirAll(filepath.Join(temporaryDir, requiredDirectory), 0o700); err != nil {
			return Report{}, errors.New("restored project layout could not be completed")
		}
	}
	if currentDigest, digestErr := digestFile(ctx, archivePath, archiveSizeLimit(limits)); digestErr != nil || currentDigest != digest {
		return Report{}, errors.New("restore archive changed while it was being read")
	}
	if err := publishDirectoryNoReplace(temporaryDir, finalDir); err != nil {
		return Report{}, errors.New("restored project could not be published without overwriting")
	}
	published = true
	if _, err := store.ProjectDirectory(manifest.Project, root); err != nil {
		if cleanupErr := os.RemoveAll(finalDir); cleanupErr != nil {
			return Report{}, errors.New("restored project failed path validation and cleanup did not complete")
		}
		return Report{}, errors.New("restored project failed the local path validation")
	}
	if err := project.RegisterProject(manifest.Project, root); err != nil {
		refs, readErr := project.LoadProjectRefsReadOnly(root)
		registered := false
		if readErr == nil {
			for _, ref := range refs {
				key, keyErr := ref.StorageKey()
				if keyErr == nil && key == manifest.ProjectKey {
					registered = true
					break
				}
			}
		}
		if !registered && readErr == nil {
			if cleanupErr := os.RemoveAll(finalDir); cleanupErr != nil {
				return Report{}, errors.New("project registration failed and restored-file cleanup did not complete")
			}
		}
		return Report{}, errors.New("restored project registry update did not complete cleanly; inspect the local project list and restore destination")
	}
	return Report{State: "restored", ProjectKey: manifest.ProjectKey, FileCount: len(manifest.Files), TotalBytes: total, ArchiveSHA256: digest}, nil
}

func validLimits(limits Limits) bool { return limits.MaxBytes > 0 && limits.MaxFiles > 0 }

func inventory(ctx context.Context, root string, limits Limits) ([]sourceFile, int64, error) {
	var files []sourceFile
	var total int64
	err := filepath.WalkDir(root, func(pathname string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("project files could not be inventoried")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if pathname == root {
			return nil
		}
		info, err := os.Lstat(pathname)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("project contains a symbolic link or unreadable entry")
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return errors.New("project contains a non-regular file")
		}
		relative, err := filepath.Rel(root, pathname)
		if err != nil {
			return errors.New("project path could not be normalized")
		}
		relative = filepath.ToSlash(relative)
		if relative == ".maintenance.lock" {
			return nil
		}
		if err := validateRelativePath(relative); err != nil {
			return err
		}
		if len(files) >= limits.MaxFiles {
			return errors.New("project file count exceeds the explicit backup limit")
		}
		remaining := limits.MaxBytes - total
		if remaining < 0 || info.Size() > remaining {
			return errors.New("project bytes exceed the explicit backup limit")
		}
		digest, size, err := hashPath(ctx, root, relative, remaining)
		if err != nil || size != info.Size() {
			return errors.New("project file changed or could not be read during backup")
		}
		total += size
		files = append(files, sourceFile{path: relative, entry: fileEntry{Path: relative, Size: size, SHA256: digest}})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].entry.Path < files[j].entry.Path })
	return files, total, nil
}

func writeArchive(ctx context.Context, projectRoot, destination string, manifest []byte, files []sourceFile) (retErr error) {
	directory := filepath.Dir(destination)
	temporary, err := os.CreateTemp(directory, ".bb-project-backup-*.tmp")
	if err != nil {
		return errors.New("backup archive could not be staged")
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			if cleanupErr := os.Remove(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				retErr = errors.Join(retErr, errors.New("private backup staging-file cleanup did not complete"))
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("backup archive permissions could not be restricted")
	}
	writer := tar.NewWriter(temporary)
	if err := writeTarMember(writer, manifestName, manifest, int64(len(manifest))); err != nil {
		return errors.New("backup manifest could not be written")
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		handle, err := openProjectFile(projectRoot, file.path)
		if err != nil {
			return errors.New("project file changed during backup")
		}
		info, statErr := handle.Stat()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() != file.entry.Size {
			_ = handle.Close()
			return errors.New("project file changed during backup")
		}
		header := tar.Header{Name: "files/" + file.entry.Path, Mode: 0o600, Size: file.entry.Size, Typeflag: tar.TypeReg,
			ModTime: time.Unix(0, 0).UTC(), Uid: 0, Gid: 0, Format: tar.FormatUSTAR}
		if err := writer.WriteHeader(&header); err != nil {
			_ = handle.Close()
			return errors.New("backup archive could not be written")
		}
		h := sha256.New()
		written, copyErr := copyBounded(ctx, io.MultiWriter(writer, h), handle, file.entry.Size)
		closeErr := handle.Close()
		if copyErr != nil || closeErr != nil || written != file.entry.Size || hex.EncodeToString(h.Sum(nil)) != file.entry.SHA256 {
			return errors.New("project file changed during backup")
		}
	}
	if err := writer.Close(); err != nil {
		return errors.New("backup archive could not be finalized")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("backup archive could not be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("backup archive could not be finalized")
	}
	if err := os.Link(temporaryPath, destination); err != nil {
		return errors.New("backup archive destination already exists or cannot be published")
	}
	if err := os.Remove(temporaryPath); err != nil {
		_ = os.Remove(destination)
		return errors.New("backup staging file could not be removed")
	}
	committed = true
	if dir, err := os.Open(directory); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func writeTarMember(writer *tar.Writer, name string, body []byte, size int64) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0).UTC(), Uid: 0, Gid: 0, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	_, err := writer.Write(body)
	return err
}

func validateArchive(ctx context.Context, archivePath string, limits Limits) (archiveManifest, string, int64, error) {
	info, err := os.Stat(archivePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > archiveSizeLimit(limits) {
		return archiveManifest{}, "", 0, errors.New("restore archive exceeds the explicit archive limit or is unavailable")
	}
	digest, err := digestFile(ctx, archivePath, archiveSizeLimit(limits))
	if err != nil {
		return archiveManifest{}, "", 0, errors.New("restore archive could not be read within its limit")
	}
	file, err := openRegularNoFollow(archivePath)
	if err != nil {
		return archiveManifest{}, "", 0, errors.New("restore archive could not be opened")
	}
	defer file.Close()
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil || header.Name != manifestName || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > maxManifestSize {
		return archiveManifest{}, "", 0, errors.New("restore archive manifest is missing or unsupported")
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(reader, maxManifestSize+1))
	if err != nil || int64(len(manifestBytes)) != header.Size || len(manifestBytes) > maxManifestSize {
		return archiveManifest{}, "", 0, errors.New("restore archive manifest is malformed")
	}
	var manifest archiveManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return archiveManifest{}, "", 0, errors.New("restore archive manifest is malformed")
	}
	canonical, err := json.Marshal(manifest)
	if err != nil || !equalBytes(canonical, manifestBytes) || manifest.Format != archiveFormat || manifest.Version != archiveVersion || len(manifest.Files) > limits.MaxFiles {
		return archiveManifest{}, "", 0, errors.New("restore archive manifest has an unsupported format")
	}
	key, err := manifest.Project.StorageKey()
	if err != nil || !projectKeyPattern.MatchString(manifest.ProjectKey) || key != manifest.ProjectKey {
		return archiveManifest{}, "", 0, errors.New("restore archive project identity is inconsistent")
	}
	var total int64
	filePaths := make(map[string]struct{}, len(manifest.Files))
	for i, entry := range manifest.Files {
		if err := validateRelativePath(entry.Path); err != nil || entry.Size < 0 || !validDigest(entry.SHA256) || (i > 0 && manifest.Files[i-1].Path >= entry.Path) {
			return archiveManifest{}, "", 0, errors.New("restore archive contains an unsafe or inconsistent file manifest")
		}
		filePaths[entry.Path] = struct{}{}
		if entry.Size > limits.MaxBytes-total {
			return archiveManifest{}, "", 0, errors.New("restore archive exceeds the explicit byte limit")
		}
		total += entry.Size
	}
	for entryPath := range filePaths {
		for parent := path.Dir(entryPath); parent != "."; parent = path.Dir(parent) {
			if _, isFile := filePaths[parent]; isFile {
				return archiveManifest{}, "", 0, errors.New("restore archive file paths conflict with a parent file")
			}
		}
	}
	if len(manifest.Files) == 0 {
		return archiveManifest{}, "", 0, errors.New("restore archive contains no project files")
	}
	for _, expected := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return archiveManifest{}, "", 0, err
		}
		header, err := reader.Next()
		if err != nil || header.Name != "files/"+expected.Path || header.Typeflag != tar.TypeReg || header.Size != expected.Size {
			return archiveManifest{}, "", 0, errors.New("restore archive members do not match the manifest")
		}
		h := sha256.New()
		read, err := copyBounded(ctx, h, reader, expected.Size)
		if err != nil || read != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
			return archiveManifest{}, "", 0, errors.New("restore archive member failed its integrity check")
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return archiveManifest{}, "", 0, errors.New("restore archive contains unexpected members")
	}
	return manifest, digest, total, nil
}

func extractArchive(ctx context.Context, archivePath, destination string, manifest archiveManifest) error {
	file, err := openRegularNoFollow(archivePath)
	if err != nil {
		return errors.New("restore archive could not be reopened")
	}
	defer file.Close()
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil || header.Name != manifestName || header.Typeflag != tar.TypeReg {
		return errors.New("restore archive changed before extraction")
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(reader, maxManifestSize+1))
	canonical, marshalErr := json.Marshal(manifest)
	if err != nil || marshalErr != nil || !equalBytes(manifestBytes, canonical) {
		return errors.New("restore archive changed before extraction")
	}
	for _, expected := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if err != nil || header.Name != "files/"+expected.Path || header.Typeflag != tar.TypeReg || header.Size != expected.Size {
			return errors.New("restore archive changed before extraction")
		}
		target := filepath.Join(destination, filepath.FromSlash(expected.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return errors.New("restore directory could not be created")
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return errors.New("restore file could not be created safely")
		}
		h := sha256.New()
		written, copyErr := copyBounded(ctx, io.MultiWriter(output, h), reader, expected.Size)
		syncErr := output.Sync()
		closeErr := output.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil || written != expected.Size || hex.EncodeToString(h.Sum(nil)) != expected.SHA256 {
			return errors.New("restore file failed its integrity check")
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		return errors.New("restore archive contains unexpected members")
	}
	return nil
}

func hashPath(ctx context.Context, root, relative string, maxBytes int64) (string, int64, error) {
	file, err := openProjectFile(root, relative)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	h := sha256.New()
	count, err := copyBounded(ctx, h, file, maxBytes)
	if err != nil || count > maxBytes {
		return "", count, errors.New("file exceeds limit or could not be read")
	}
	return hex.EncodeToString(h.Sum(nil)), count, nil
}

func digestFile(ctx context.Context, filename string, maxBytes int64) (string, error) {
	file, err := openRegularNoFollow(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	count, err := copyBounded(ctx, h, file, maxBytes)
	if err != nil || count > maxBytes {
		return "", errors.New("file exceeds limit or could not be read")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyBounded(ctx context.Context, destination io.Writer, source io.Reader, limit int64) (int64, error) {
	if limit < 0 {
		return 0, errors.New("negative copy limit")
	}
	buffer := make([]byte, 32*1024)
	var total int64
	noProgress := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		remaining := limit - total
		if remaining <= 0 {
			var probe [1]byte
			n, err := source.Read(probe[:])
			if n > 0 {
				return total + int64(n), errors.New("copy limit exceeded")
			}
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			if err != nil {
				return total, err
			}
			noProgress++
			if noProgress >= 100 {
				return total, io.ErrNoProgress
			}
			continue
		}
		if int64(len(buffer)) > remaining {
			buffer = buffer[:remaining]
		}
		n, readErr := source.Read(buffer)
		if n == 0 && readErr == nil {
			noProgress++
			if noProgress >= 100 {
				return total, io.ErrNoProgress
			}
			continue
		}
		noProgress = 0
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validateRelativePath(value string) error {
	if value == "" || strings.ContainsAny(value, "\\:\x00") || strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." {
		return errors.New("archive contains an unsafe path")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." || strings.TrimRight(component, ". ") != component {
			return errors.New("archive contains an unsafe path")
		}
		for _, r := range component {
			if r < 0x20 || r == 0x7f {
				return errors.New("archive contains an unsafe path")
			}
		}
		base := strings.ToUpper(strings.SplitN(component, ".", 2)[0])
		if reservedWindowsName(base) {
			return errors.New("archive contains a reserved path component")
		}
	}
	if filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))) != value || value == ".maintenance.lock" || value == "vault" || value == "behavior-receipts" {
		return errors.New("archive contains a non-canonical path")
	}
	return nil
}

func reservedWindowsName(base string) bool {
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	runes := []rune(base)
	if len(runes) != 4 || (!strings.HasPrefix(base, "COM") && !strings.HasPrefix(base, "LPT")) {
		return false
	}
	switch string(runes[3]) {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	default:
		return false
	}
}

func archiveSizeLimit(limits Limits) int64 {
	// Tar headers and padding are bounded by 1024 bytes per member, plus a
	// bounded manifest and end markers. Saturate on overflow.
	maxInt64 := int64(^uint64(0) >> 1)
	if limits.MaxBytes > maxInt64-maxManifestSize-2048 {
		return maxInt64
	}
	remaining := maxInt64 - limits.MaxBytes - maxManifestSize - 2048
	files := int64(limits.MaxFiles)
	if files > remaining/1024 {
		return maxInt64
	}
	return limits.MaxBytes + maxManifestSize + 2048 + files*1024
}

func prepareNewFilePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("backup archive path is invalid")
	}
	if err := rejectSymlinkComponents(filepath.Dir(absolute)); err != nil {
		return "", errors.New("backup directory contains a symbolic link or is unavailable")
	}
	parentInfo, err := os.Stat(filepath.Dir(absolute))
	if err != nil || !parentInfo.IsDir() {
		return "", errors.New("backup directory must already exist")
	}
	if _, err := os.Lstat(absolute); err == nil {
		return "", errors.New("refusing to overwrite an existing backup archive")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("backup archive path could not be checked")
	}
	return absolute, nil
}

func existingRegularPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil || rejectSymlinkComponents(absolute) != nil {
		return "", errors.New("path contains a symbolic link or is unavailable")
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	return absolute, nil
}

func rejectSymlinkComponents(value string) error {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimLeft(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symbolic link or inaccessible path component")
		}
	}
	return nil
}

func pathWithin(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
