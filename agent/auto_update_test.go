package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestPendingRavelUpdateDoesNotOverwriteRollbackBinary(t *testing.T) {
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "ravel")
	if err := os.WriteFile(binaryPath, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath+".previous", []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePendingRavelUpdate(binaryPath, PendingRavelUpdate{TargetVersion: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}

	err := installRavelRelease(context.Background(), githubRelease{TagName: "v1.2.3"}, binaryPath, "v1.2.2", "request-2")
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(binaryPath + ".previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(previous) != "old-binary" {
		t.Fatalf("rollback binary was overwritten: %q", previous)
	}
}

func TestVerifyReleaseAssetDigest(t *testing.T) {
	data := []byte("verified-release")
	digest := sha256.Sum256(data)
	if err := verifyReleaseAssetDigest(data, "sha256:"+hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseAssetDigest([]byte("tampered"), "sha256:"+hex.EncodeToString(digest[:])); err == nil {
		t.Fatal("tampered release was accepted")
	}
}

func TestRavelVersionOutputMustMatchReleaseTag(t *testing.T) {
	if !ravelVersionOutputMatches("ravel v1.2.3 (Ravel integrated node runtime)", "v1.2.3") {
		t.Fatal("matching Ravel version output was rejected")
	}
	if ravelVersionOutputMatches("ravel v1.2.2 (Ravel integrated node runtime)", "v1.2.3") {
		t.Fatal("mismatched Ravel version output was accepted")
	}
}

func TestCurrentRavelReleaseTargetUsesBuildArchitecture(t *testing.T) {
	previousOS := runtimeGOOS
	previousArch := runtimeGOARCH
	previousSetting := readCurrentBuildSetting
	t.Cleanup(func() {
		runtimeGOOS = previousOS
		runtimeGOARCH = previousArch
		readCurrentBuildSetting = previousSetting
	})

	runtimeGOOS = "linux"
	runtimeGOARCH = "arm"
	readCurrentBuildSetting = func(name string) string {
		if name == "GOARM" {
			return "6"
		}
		return ""
	}
	target, err := currentRavelReleaseTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.AssetName != "v2node-linux-arm32-v6.zip" || target.BinaryName != "v2node" {
		t.Fatalf("unexpected ARM target: %+v", target)
	}

	runtimeGOARCH = "mipsle"
	readCurrentBuildSetting = func(name string) string {
		if name == "GOMIPS" {
			return "softfloat"
		}
		return ""
	}
	target, err = currentRavelReleaseTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.AssetName != "v2node-linux-mips32le.zip" || target.BinaryName != "v2node_softfloat" {
		t.Fatalf("unexpected MIPS target: %+v", target)
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
