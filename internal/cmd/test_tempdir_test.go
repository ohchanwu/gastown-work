package cmd

import (
	"path/filepath"
	"testing"
)

func canonicalTestTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return resolved
}
