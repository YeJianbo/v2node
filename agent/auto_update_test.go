package agent

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReplaceRavelBinaryKeepsPreviousVersion(t *testing.T) {
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "ravel")
	if err := os.WriteFile(binaryPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("v2node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("new-binary")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if err := replaceRavelBinary(binaryPath, archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(binaryPath + ".previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "new-binary" || string(previous) != "old-binary" {
		t.Fatalf("unexpected binaries: current=%q previous=%q", current, previous)
	}
}

func TestNormalizeAutoUpdateConfig(t *testing.T) {
	config := normalizeAutoUpdateConfig(AutoUpdateConfig{Enabled: true})
	if config.IntervalSeconds != 86400 || config.Repo != "YeJianbo/v2node" {
		t.Fatalf("unexpected normalized config: %+v", config)
	}
}
