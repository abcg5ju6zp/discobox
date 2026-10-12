package gitcache

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// An imported origin is a bare repository out of an archive: every byte of
// it, its configuration included, is whatever the archive's author wrote. So
// git is never run in it — a repository's own configuration names programs
// git runs, and this process is root on the pool host. Its refs are read as
// the files they are, and its objects are lent to the cache as an alternate
// for the one command that copies them in.

// maxImportedRefFile bounds one ref file, and packed-refs, of an imported
// repository.
const (
	maxImportedRefFile    = 1 << 10
	maxImportedPackedRefs = 256 << 20
)

// importedRepository is what an imported origin holds, read without git.
type importedRepository struct {
	// refs is every ref under refs/, by full name, and the object it names.
	refs map[string]string
	// branch is the branch HEAD names, empty when it names none.
	branch string
	// objects is the repository's object directory.
	objects string
}

// readImportedRepository reads the refs and HEAD of the bare repository at
// src and checks that its object store is plain files and directories: a
// link in it, or an alternates file, would lend the cache objects from
// somewhere else on this host.
func readImportedRepository(src string) (importedRepository, error) {
	root, err := os.OpenRoot(src)
	if err != nil {
		return importedRepository{}, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat("reftable"); err == nil {
		return importedRepository{}, errors.New("the imported origin keeps its refs in a format this pool does not read")
	}
	repo := importedRepository{refs: map[string]string{}, objects: filepath.Join(src, "objects")}

	// Packed refs first: a loose ref of the same name is the newer value.
	if data, err := readPlainFile(root, "packed-refs", maxImportedPackedRefs); err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" || line[0] == '#' || line[0] == '^' {
				continue
			}
			object, ref, ok := strings.Cut(line, " ")
			if !ok || !validObjectID(object) || !ValidRef(ref) {
				return importedRepository{}, errors.New("the imported origin's packed refs hold something other than refs")
			}
			repo.refs[ref] = object
		}
		if err := scanner.Err(); err != nil {
			return importedRepository{}, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return importedRepository{}, err
	}
	err = fs.WalkDir(root.FS(), "refs", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && name == "refs" {
				return fs.SkipAll
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := readPlainFile(root, name, maxImportedRefFile)
		if err != nil {
			return err
		}
		object := strings.TrimSpace(string(data))
		if !validObjectID(object) || !ValidRef(name) {
			return fmt.Errorf("the imported origin's ref %q is not a ref at an object", name)
		}
		repo.refs[name] = object
		return nil
	})
	if err != nil {
		return importedRepository{}, err
	}
	if data, err := readPlainFile(root, "HEAD", maxImportedRefFile); err == nil {
		if target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/"); ok && ValidRef("refs/heads/"+target) {
			repo.branch = target
		}
	}

	if _, err := root.Lstat("objects/info/alternates"); err == nil {
		return importedRepository{}, errors.New("the imported origin borrows objects from another repository")
	}
	err = fs.WalkDir(root.FS(), "objects", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("the imported origin's %s is not a plain file", name)
		}
		return nil
	})
	if err != nil {
		return importedRepository{}, err
	}
	return repo, nil
}

// readPlainFile reads a regular file beneath root, refusing a link or
// anything over limit bytes.
func readPlainFile(root *os.Root, name string, limit int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("the imported origin's %s is not a plain file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the imported origin's %s is too large", name)
	}
	return data, nil
}
