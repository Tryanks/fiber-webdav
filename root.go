package webdav

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RootFileSystem is a FileSystem implementation based on os.Root
type RootFileSystem struct {
	root     *os.Root
	rootPath string
}

// NewRootFileSystem creates a new RootFileSystem
func NewRootFileSystem(rootDir string) (*RootFileSystem, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, err
	}
	return &RootFileSystem{root: root, rootPath: rootDir}, nil
}

// Close closes the root directory
func (rfs *RootFileSystem) Close() error {
	return rfs.root.Close()
}

// Mkdir creates a new directory within the root directory
func (rfs *RootFileSystem) Mkdir(_ context.Context, name string, perm os.FileMode) error {
	return rfs.root.Mkdir(name, perm)
}

// OpenFile opens a file within the root directory
func (rfs *RootFileSystem) OpenFile(_ context.Context, name string, flag int, perm os.FileMode) (File, error) {
	osFile, err := rfs.root.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return &rootFile{file: osFile}, nil
}

// RemoveAll removes a file or directory and all its contents within the root directory
func (rfs *RootFileSystem) RemoveAll(_ context.Context, name string) error {
	// os.Root currently doesn't provide RemoveAll method directly, we need to implement it recursively
	info, err := rfs.root.Stat(name)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		return rfs.root.Remove(name)
	}

	file, err := rfs.root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	entries, err := file.ReadDir(-1)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		fullPath := filepath.Join(name, entry.Name())
		if err := rfs.RemoveAll(context.Background(), fullPath); err != nil {
			return err
		}
	}

	return rfs.root.Remove(name)
}

// Rename renames a file or directory within the root directory
func (rfs *RootFileSystem) Rename(_ context.Context, oldName, newName string) error {
	//return rfs.root.Rename(oldName, newName) // TODO: Will available in Go 1.24.2, now 1.24.1

	oldPath, err := rfs.Path(oldName)
	if err != nil {
		return err
	}
	newPath, err := rfs.Path(newName)
	if err != nil {
		return err
	}
	return os.Rename(oldPath, newPath)
}

// Stat gets information about a file or directory within the root directory
func (rfs *RootFileSystem) Stat(_ context.Context, name string) (os.FileInfo, error) {
	return rfs.root.Stat(name)
}

// Path returns the absolute path of the specified file, reports error when path escape occurs
func (rfs *RootFileSystem) Path(path string) (string, error) {
	path = filepath.Join(rfs.rootPath, path)

	cleanedPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanedPath) {
		cleanedPath = filepath.Join(rfs.rootPath, cleanedPath)
	}

	relPath, err := filepath.Rel(rfs.rootPath, cleanedPath)
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(relPath, "..") {
		return "", fmt.Errorf("path %s escapes root directory", path)
	}

	return cleanedPath, nil
}

// rootFile implements the File interface, wrapping *os.File
type rootFile struct {
	file *os.File
}

func (f *rootFile) Read(p []byte) (n int, err error) {
	return f.file.Read(p)
}

func (f *rootFile) Write(p []byte) (n int, err error) {
	return f.file.Write(p)
}

func (f *rootFile) Seek(offset int64, whence int) (int64, error) {
	return f.file.Seek(offset, whence)
}

func (f *rootFile) Close() error {
	return f.file.Close()
}

// Readdir returns a list of files in the directory
func (f *rootFile) Readdir(count int) ([]fs.FileInfo, error) {
	return f.file.Readdir(count)
}

// Stat returns information about the file
func (f *rootFile) Stat() (fs.FileInfo, error) {
	return f.file.Stat()
}
