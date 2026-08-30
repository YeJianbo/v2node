package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	config := normalizeAutoUpdateConfig(AutoUpdateConfig{
		Enabled:       true,
		RequestID:     "request-1",
		TargetVersion: "v0.5.4-ravel4",
	})
	if config.IntervalSeconds != 86400 || config.Repo != "YeJianbo/v2node" {
		t.Fatalf("unexpected normalized config: %+v", config)
	}
	if config.RequestID != "request-1" || config.TargetVersion != "v0.5.4-ravel4" {
		t.Fatalf("manual update request was not preserved: %+v", config)
	}
}

func TestFetchGitHubReleaseByTag(t *testing.T) {
	requestedPath := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestedPath = request.URL.Path
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"tag_name":"v0.5.4-ravel4","assets":[]}`))
	}))
	defer server.Close()

	previousAPIBaseURL := githubAPIBaseURL
	githubAPIBaseURL = server.URL
	t.Cleanup(func() { githubAPIBaseURL = previousAPIBaseURL })

	release, err := fetchGitHubRelease(context.Background(), "YeJianbo/v2node", "v0.5.4-ravel4")
	if err != nil {
		t.Fatal(err)
	}
	if release.TagName != "v0.5.4-ravel4" {
		t.Fatalf("unexpected release: %+v", release)
	}
	if requestedPath != "/repos/YeJianbo/v2node/releases/tags/v0.5.4-ravel4" {
		t.Fatalf("unexpected release path: %s", requestedPath)
	}
}
