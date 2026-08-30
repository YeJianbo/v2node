package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

type AutoUpdateConfig struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds int    `json:"interval_seconds"`
	Repo            string `json:"repo"`
	RequestID       string `json:"request_id"`
	TargetVersion   string `json:"target_version"`
	RequestedAt     int64  `json:"requested_at"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func normalizeAutoUpdateConfig(config AutoUpdateConfig) AutoUpdateConfig {
	if config.IntervalSeconds < 3600 {
		config.IntervalSeconds = 86400
	}
	if config.Repo == "" {
		config.Repo = "YeJianbo/v2node"
	}
	return config
}

func AutoUpdateInterval(config AutoUpdateConfig) time.Duration {
	return time.Duration(normalizeAutoUpdateConfig(config).IntervalSeconds) * time.Second
}

func CheckAndInstallRavelUpdate(ctx context.Context, config AutoUpdateConfig, currentVersion, binaryPath string) (string, bool, error) {
	config = normalizeAutoUpdateConfig(config)
	if !config.Enabled {
		return "", false, nil
	}
	currentVersion = strings.TrimSpace(currentVersion)
	if !semver.IsValid(currentVersion) {
		return "", false, fmt.Errorf("current Ravel version %q is not a release version", currentVersion)
	}

	release, err := fetchGitHubRelease(ctx, config.Repo, "")
	if err != nil {
		return "", false, err
	}
	if !semver.IsValid(release.TagName) {
		return "", false, fmt.Errorf("latest release has invalid version %q", release.TagName)
	}
	if semver.Compare(release.TagName, currentVersion) <= 0 {
		return release.TagName, false, nil
	}
	if err := installRavelRelease(ctx, release, binaryPath); err != nil {
		return release.TagName, false, err
	}
	return release.TagName, true, nil
}

var githubAPIBaseURL = "https://api.github.com"

func CheckAndInstallRequestedRavelUpdate(ctx context.Context, config AutoUpdateConfig, currentVersion, binaryPath string) (string, bool, error) {
	config = normalizeAutoUpdateConfig(config)
	if strings.TrimSpace(config.RequestID) == "" {
		return "", false, nil
	}

	release, err := fetchGitHubRelease(ctx, config.Repo, strings.TrimSpace(config.TargetVersion))
	if err != nil {
		return "", false, err
	}
	if !semver.IsValid(release.TagName) {
		return "", false, fmt.Errorf("requested release has invalid version %q", release.TagName)
	}
	if strings.TrimSpace(currentVersion) == release.TagName {
		return release.TagName, false, nil
	}
	if err := installRavelRelease(ctx, release, binaryPath); err != nil {
		return release.TagName, false, err
	}
	return release.TagName, true, nil
}

func fetchGitHubRelease(ctx context.Context, repo, targetVersion string) (githubRelease, error) {
	var release githubRelease
	endpoint := strings.TrimRight(githubAPIBaseURL, "/") + "/repos/" + repo + "/releases/latest"
	if targetVersion != "" {
		endpoint = strings.TrimRight(githubAPIBaseURL, "/") + "/repos/" + repo + "/releases/tags/" + url.PathEscape(targetVersion)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "v2node-ravel-updater")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return release, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return release, fmt.Errorf("GitHub release API returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&release); err != nil {
		return release, err
	}
	return release, nil
}

func installRavelRelease(ctx context.Context, release githubRelease, binaryPath string) error {
	assetName, err := ravelReleaseAssetName()
	if err != nil {
		return err
	}
	assetURL := ""
	for _, asset := range release.Assets {
		if asset.Name == assetName {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}
	if assetURL == "" {
		return fmt.Errorf("release asset %s was not found", assetName)
	}

	archive, err := downloadUpdateAsset(ctx, assetURL)
	if err != nil {
		return err
	}
	if err := replaceRavelBinary(binaryPath, archive); err != nil {
		return err
	}
	return nil
}

func ravelReleaseAssetName() (string, error) {
	arch := map[string]string{
		"386":      "32",
		"amd64":    "64",
		"arm":      "arm32-v7a",
		"arm64":    "arm64-v8a",
		"mips":     "mips32",
		"mipsle":   "mips32le",
		"mips64":   "mips64",
		"mips64le": "mips64le",
		"ppc64":    "ppc64",
		"ppc64le":  "ppc64le",
		"riscv64":  "riscv64",
		"s390x":    "s390x",
	}[runtime.GOARCH]
	if arch == "" || runtime.GOOS != "linux" {
		return "", fmt.Errorf("Ravel auto update does not support %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return "v2node-linux-" + arch + ".zip", nil
}

func downloadUpdateAsset(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "v2node-ravel-updater")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download release asset returned %s", response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, 128<<20))
}

func replaceRavelBinary(binaryPath string, archive []byte) error {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	var binary *zip.File
	for _, entry := range reader.File {
		if filepath.Base(entry.Name) == "v2node" && !entry.FileInfo().IsDir() {
			binary = entry
			break
		}
	}
	if binary == nil {
		return fmt.Errorf("release archive does not contain v2node")
	}

	source, err := binary.Open()
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(binaryPath), ".ravel-update-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := io.Copy(temporary, source); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	backupPath := binaryPath + ".previous"
	_ = os.Remove(backupPath)
	if err := os.Rename(binaryPath, backupPath); err != nil {
		return fmt.Errorf("backup current Ravel binary: %w", err)
	}
	if err := os.Rename(temporaryPath, binaryPath); err != nil {
		_ = os.Rename(backupPath, binaryPath)
		return fmt.Errorf("activate Ravel update: %w", err)
	}
	return nil
}
