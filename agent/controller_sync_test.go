package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncRejectsMissingManagedNodeData(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	original := []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":1,"ApiKey":"secret","Timeout":15}]}`)
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	controller := newSyncTestController(t, directory, `{"probe":{"relay":{"rules":[]}}}`)
	controller.ConfigFile = configPath
	if _, _, err := controller.Sync(); err == nil {
		t.Fatal("response without data was accepted")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("missing data response changed the local configuration")
	}
}

func TestSyncTreatsAuthoritativeEmptyDataAsManagedNodeRemoval(t *testing.T) {
	directory := t.TempDir()
	controller := newSyncTestController(t, directory, `{
		"data":[],
		"config_revision":"empty-revision",
		"authoritative":true,
		"probe":{"relay":{"rules":[]}}
	}`)
	writeSyncTestKey(t, controller.KeyFile)
	existing := map[string]any{
		"Log": map[string]any{"Level": "warning", "Output": "", "Access": "none"},
		"Nodes": []NodeConfig{
			{APIHost: "https://manual.example", NodeID: 1, APIKey: "manual", Timeout: 15},
			{APIHost: "https://panel.example", NodeID: 2, APIKey: "managed", Timeout: 15},
		},
	}
	raw, _ := json.Marshal(existing)
	if err := os.WriteFile(controller.ConfigFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.ManagedFile, []byte(`["2"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, count, err := controller.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if !changed || count != 0 {
		t.Fatalf("unexpected sync result: changed=%v count=%d", changed, count)
	}
	encrypted, err := os.ReadFile(controller.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := controller.decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Nodes []NodeConfig `json:"Nodes"`
	}
	if err := json.Unmarshal(plain, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].APIHost != "https://manual.example" {
		t.Fatalf("managed removal did not preserve only manual nodes: %+v", result.Nodes)
	}
}

func TestSyncDoesNotTurnFailedRevisionIntoSuccess(t *testing.T) {
	directory := t.TempDir()
	response := `{
		"data":[{"ApiHost":"https://panel.example","NodeID":1,"ApiKey":"secret","Timeout":15}],
		"config_revision":"revision-1",
		"authoritative":true,
		"probe":{"relay":{"rules":[]}}
	}`
	controller := newSyncTestController(t, directory, response)
	writeSyncTestKey(t, controller.KeyFile)
	config := `{"Log":{"Level":"warning","Output":"","Access":"none"},"Nodes":[{"ApiHost":"https://panel.example","NodeID":1,"ApiKey":"secret","Timeout":15}]}`
	if err := os.WriteFile(controller.ConfigFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.ManagedFile, []byte(`["1"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	managedBefore, err := os.Stat(controller.ManagedFile)
	if err != nil {
		t.Fatal(err)
	}
	controller.setDesiredRevision("revision-1")
	controller.MarkConfigApply("failed", "port conflict")

	changed, _, err := controller.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("unchanged failed revision unexpectedly requested a reload")
	}
	managedAfter, err := os.Stat(controller.ManagedFile)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(managedBefore, managedAfter) {
		t.Fatal("unchanged managed node state was rewritten")
	}
	state := controller.currentApplyState()
	if state.Status != "failed" || state.Error != "port conflict" {
		t.Fatalf("failed apply state was overwritten: %+v", state)
	}
}

func TestSyncCompletesRevisionWithoutUnnecessaryNodeReload(t *testing.T) {
	directory := t.TempDir()
	response := `{
		"data":[{"ApiHost":"https://panel.example","NodeID":1,"ApiKey":"secret","Timeout":15}],
		"config_revision":"revision-2",
		"authoritative":true,
		"probe":{"relay":{"rules":[]}}
	}`
	controller := newSyncTestController(t, directory, response)
	writeSyncTestKey(t, controller.KeyFile)
	config := `{"Log":{"Level":"warning","Output":"","Access":"none"},"Nodes":[{"ApiHost":"https://panel.example","NodeID":1,"ApiKey":"secret","Timeout":15}]}`
	if err := os.WriteFile(controller.ConfigFile, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(controller.ManagedFile, []byte(`["1"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	controller.setDesiredRevision("revision-1")
	controller.MarkConfigApply("success", "")

	changed, _, err := controller.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("metadata-only revision change requested a node reload")
	}
	state := controller.currentApplyState()
	if state.Status != "success" || state.AppliedRevision != "revision-2" {
		t.Fatalf("revision was not completed: %+v", state)
	}
}

func TestRestoreLastGoodConfigSupportsOfflineStartup(t *testing.T) {
	directory := t.TempDir()
	controller := &Controller{ConfigFile: filepath.Join(directory, "config.json")}
	if err := os.WriteFile(controller.ConfigFile, []byte(`{"Nodes":`), 0o600); err != nil {
		t.Fatal(err)
	}
	lastGood := `{"Nodes":[{"ApiHost":"https://panel.example","NodeID":1},{"ApiHost":"https://panel.example","NodeID":2}]}`
	if err := os.WriteFile(controller.LastGoodConfigPath(), []byte(lastGood), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := controller.RestoreLastGoodConfig(); err != nil {
		t.Fatal(err)
	}
	count, err := controller.LocalNodeCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("unexpected restored node count: %d", count)
	}
}

func newSyncTestController(t *testing.T, directory, responseBody string) *Controller {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(State{
		PanelURL:     server.URL,
		MachineToken: "test-token",
		MachineID:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{
		Client:      client,
		ConfigFile:  filepath.Join(directory, "config.json"),
		KeyFile:     filepath.Join(directory, "config.key"),
		ManagedFile: filepath.Join(directory, "managed-nodes.json"),
	}
}

func writeSyncTestKey(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
}
