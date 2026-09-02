package rig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const rigRegistrationJournalVersion = 1

type rigRegistrationFileSnapshot struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Mode    os.FileMode `json:"mode,omitempty"`
	Data    []byte      `json:"data,omitempty"`
}

type rigRegistrationJournal struct {
	Version int                           `json:"version"`
	Name    string                        `json:"name"`
	Token   string                        `json:"token"`
	Files   []rigRegistrationFileSnapshot `json:"files"`
}

func rigRegistrationJournalPath(townRoot, name string) string {
	return filepath.Join(townRoot, ".runtime", "rig-registration", name+".json")
}

func beginRigRegistrationJournal(townRoot, name, token string) error {
	if name == "" || token == "" {
		return fmt.Errorf("registration name and token are required")
	}
	journalPath := rigRegistrationJournalPath(townRoot, name)
	if _, err := os.Stat(journalPath); err == nil {
		return fmt.Errorf("registration journal for %q already exists", name)
	} else if !os.IsNotExist(err) {
		return err
	}

	rigPath := filepath.Join(townRoot, name)
	paths := []string{
		filepath.Join(rigPath, ".repo.git", "config"),
		filepath.Join(rigPath, "mayor", "rig", ".git", "config"),
		filepath.Join(rigPath, "config.json"),
	}
	journal := rigRegistrationJournal{Version: rigRegistrationJournalVersion, Name: name, Token: token}
	for _, path := range paths {
		snapshot, err := snapshotRigRegistrationFile(rigPath, path)
		if err != nil {
			return err
		}
		journal.Files = append(journal.Files, snapshot)
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(journalPath), 0o700); err != nil {
		return err
	}
	return writeRigRegistrationFileDurable(journalPath, append(data, '\n'), 0o600)
}

func snapshotRigRegistrationFile(rigPath, path string) (rigRegistrationFileSnapshot, error) {
	rel, err := filepath.Rel(rigPath, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return rigRegistrationFileSnapshot{}, fmt.Errorf("registration snapshot path escapes rig: %s", path)
	}
	snapshot := rigRegistrationFileSnapshot{Path: rel}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() {
		return snapshot, fmt.Errorf("registration snapshot target is not a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot, err
	}
	snapshot.Existed = true
	snapshot.Mode = info.Mode().Perm()
	snapshot.Data = data
	return snapshot, nil
}

func readRigRegistrationJournal(townRoot, name string) (rigRegistrationJournal, error) {
	data, err := os.ReadFile(rigRegistrationJournalPath(townRoot, name))
	if err != nil {
		return rigRegistrationJournal{}, err
	}
	var journal rigRegistrationJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return rigRegistrationJournal{}, err
	}
	if journal.Version != rigRegistrationJournalVersion || journal.Name != name || journal.Token == "" {
		return rigRegistrationJournal{}, fmt.Errorf("invalid registration journal for %q", name)
	}
	return journal, nil
}

func restoreRigRegistrationJournal(townRoot, name, token string) error {
	journal, err := readRigRegistrationJournal(townRoot, name)
	if err != nil {
		return err
	}
	if token != "" && journal.Token != token {
		return fmt.Errorf("registration journal for %q is owned by another operation", name)
	}
	rigPath := filepath.Join(townRoot, name)
	for _, snapshot := range journal.Files {
		path, err := rigRegistrationSnapshotPath(rigPath, snapshot.Path)
		if err != nil {
			return err
		}
		if snapshot.Existed {
			if err := writeRigRegistrationFileDurable(path, snapshot.Data, snapshot.Mode); err != nil {
				return fmt.Errorf("restoring %s: %w", snapshot.Path, err)
			}
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing newly created %s: %w", snapshot.Path, err)
		}
		if err := syncRigRegistrationDirectory(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return retireRigRegistrationJournal(townRoot, name, journal.Token)
}

func rigRegistrationSnapshotPath(rigPath, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || filepath.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid registration snapshot path %q", rel)
	}
	return filepath.Join(rigPath, rel), nil
}

func retireRigRegistrationJournal(townRoot, name, token string) error {
	journal, err := readRigRegistrationJournal(townRoot, name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if journal.Token != token {
		return fmt.Errorf("registration journal for %q is owned by another operation", name)
	}
	path := rigRegistrationJournalPath(townRoot, name)
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncRigRegistrationDirectory(filepath.Dir(path))
}

func writeRigRegistrationFileDurable(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".registration-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return syncRigRegistrationDirectory(dir)
}

func syncRigRegistrationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func abortRigRegistrationJournal(townRoot, name, token string, cause error) error {
	return errors.Join(cause, restoreRigRegistrationJournal(townRoot, name, token))
}
