package remoteobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrSpoolFull = errors.New("collector spool quota exhausted")

// Spool is owned by one collector process. The caller serializes access.
type Spool struct {
	path     string
	maxBytes int64
	maxFiles int
	lock     *os.File
}

type Pending struct {
	Name string
	Data []byte
}

func OpenSpool(path string, maxBytes int64, maxFiles int) (*Spool, error) {
	if !filepath.IsAbs(path) || maxBytes < MaxBody || maxBytes > 16<<30 || maxFiles < 1 || maxFiles > 10000 {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("collector spool must be a private directory")
	}
	lock, err := os.OpenFile(filepath.Join(path, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockSpool(lock); err != nil {
		lock.Close()
		return nil, errors.New("collector spool is already in use")
	}
	return &Spool{path: path, maxBytes: maxBytes, maxFiles: maxFiles, lock: lock}, nil
}

func (s *Spool) Close() error { return s.lock.Close() }

func (s *Spool) entries() ([]os.DirEntry, int64, error) {
	children, err := os.ReadDir(s.path)
	if err != nil {
		return nil, 0, err
	}
	var used int64
	pending := children[:0]
	for _, child := range children {
		if !strings.HasSuffix(child.Name(), ".observation") {
			continue
		}
		info, err := child.Info()
		if err != nil {
			return nil, 0, err
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > MaxBody {
			return nil, 0, ErrInvalid
		}
		used += info.Size()
		pending = append(pending, child)
	}
	return pending, used, nil
}

func (s *Spool) Add(item Observation, body []byte) error {
	if !ValidID(item.ID) || len(body) == 0 || len(body) > MaxBody {
		return ErrInvalid
	}
	entries, used, err := s.entries()
	if err != nil {
		return err
	}
	if len(entries) >= s.maxFiles || used+int64(len(body)) > s.maxBytes {
		return ErrSpoolFull
	}
	file, err := os.OpenFile(filepath.Join(s.path, ".pending"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(body)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s.observation", item.ObservedAt.UnixNano(), item.ID)
	if err := os.Rename(file.Name(), filepath.Join(s.path, name)); err != nil {
		return err
	}
	return s.sync()
}

func (s *Spool) Next() (Pending, int, error) {
	entries, _, err := s.entries()
	if err != nil || len(entries) == 0 {
		return Pending{}, 0, err
	}
	name := entries[0].Name()
	body, err := os.ReadFile(filepath.Join(s.path, name))
	return Pending{Name: name, Data: body}, len(entries), err
}

func (s *Spool) Remove(item Pending) error {
	if filepath.Base(item.Name) != item.Name || !strings.HasSuffix(item.Name, ".observation") {
		return ErrInvalid
	}
	if err := os.Remove(filepath.Join(s.path, item.Name)); err != nil {
		return err
	}
	return s.sync()
}

func (s *Spool) sync() error {
	dir, err := os.Open(s.path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
