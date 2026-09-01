package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wyx2685/v2node/common/crypt"
	"github.com/wyx2685/v2node/node"
)

const configPath = "/api/v1/server/machine/v2nodeConfig"
const statusPath = "/api/v1/server/machine/push"
const restartAckPath = "/api/v1/server/machine/restartAck"
const updateAckPath = "/api/v1/server/machine/updateAck"

type NodeConfig struct {
	APIHost string `json:"ApiHost"`
	NodeID  int    `json:"NodeID"`
	APIKey  string `json:"ApiKey"`
	Timeout int    `json:"Timeout"`
}

type ConfigResponse struct {
	Data               *[]NodeConfig `json:"data"`
	ConfigRevision     string        `json:"config_revision"`
	ConfigSchema       int           `json:"config_schema"`
	Authoritative      bool          `json:"authoritative"`
	RestartToken       string        `json:"restart_node_token"`
	LegacyRestartToken string        `json:"restart_v2node_token"`
	Probe              *ProbeConfig  `json:"probe"`
}

type ProbeConfig struct {
	AutoUpdate     AutoUpdateConfig     `json:"auto_update"`
	Relay          *RelayConfig         `json:"relay"`
	NetworkQuality NetworkQualityConfig `json:"network_quality"`
	RuntimeTask    *RuntimeTask         `json:"runtime_task"`
}

type ConfigApplyState struct {
	DesiredRevision string `json:"desired_revision"`
	AppliedRevision string `json:"applied_revision"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	UpdatedAt       int64  `json:"updated_at"`
}

type Controller struct {
	Client      *Client
	ConfigFile  string
	KeyFile     string
	ManagedFile string
	Relay       *RelayManager
	qualityMu   sync.RWMutex
	quality     NetworkQualityConfig
	updateMu    sync.RWMutex
	autoUpdate  AutoUpdateConfig
	applyMu     sync.RWMutex
	applyState  ConfigApplyState
	runtimeMu   sync.RWMutex
	runtimeTask *RuntimeTask
}

func LoadState(path string) (State, error) {
	var state State
	raw, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Protocol == "" {
		state.Protocol = protocol
	}
	if state.SyncInterval <= 0 {
		state.SyncInterval = 30
	}
	if state.StatusInterval <= 0 {
		state.StatusInterval = 5
	}
	return state, nil
}

func (c *Controller) Sync() (bool, int, error) {
	var response ConfigResponse
	query := url.Values{"t": []string{strconv.FormatInt(time.Now().Unix(), 10)}}
	if err := c.Client.Get(configPath, query, &response); err != nil {
		return false, 0, err
	}
	if response.Data == nil {
		return false, 0, fmt.Errorf("panel response omitted managed node data")
	}
	if response.Probe == nil || response.Probe.Relay == nil {
		return false, len(*response.Data), fmt.Errorf("panel response omitted relay configuration")
	}
	desired := *response.Data
	revision := strings.TrimSpace(response.ConfigRevision)
	if revision == "" {
		revision = configRevision(desired, *response.Probe.Relay)
	}
	revisionChanged := c.setDesiredRevision(revision)

	relayChanged := false
	if c.Relay != nil {
		var err error
		relayChanged, err = c.Relay.Sync(*response.Probe.Relay)
		if err != nil {
			c.MarkConfigApply("failed", err.Error())
			return false, len(desired), fmt.Errorf("apply relay configuration: %w", err)
		}
	}
	c.setNetworkQualityConfig(response.Probe.NetworkQuality)
	c.setAutoUpdateConfig(response.Probe.AutoUpdate)
	c.setRuntimeTask(response.Probe.RuntimeTask)
	nodeConfigChanged, err := c.writeMergedConfig(desired)
	if err != nil {
		c.MarkConfigApply("failed", err.Error())
		return false, len(desired), err
	}
	restartToken := response.RestartToken
	if restartToken == "" {
		restartToken = response.LegacyRestartToken
	}
	restartRequested := restartToken != ""
	if restartRequested {
		var ack map[string]any
		if err := c.Client.Post(restartAckPath, map[string]string{"restart_token": restartToken}, &ack); err != nil {
			return false, len(desired), fmt.Errorf("acknowledge restart: %w", err)
		}
	}
	state := c.currentApplyState()
	if nodeConfigChanged || restartRequested || state.Status == "" {
		c.MarkConfigApply("applying", "")
	} else if relayChanged || revisionChanged {
		if state.Status == "partial" {
			c.MarkConfigApply("partial", state.Error)
		} else {
			c.MarkConfigApply("success", "")
		}
	}
	return nodeConfigChanged || restartRequested, len(desired), nil
}

func configRevision(nodes []NodeConfig, relay RelayConfig) string {
	raw, _ := json.Marshal(struct {
		Nodes []NodeConfig `json:"nodes"`
		Relay RelayConfig  `json:"relay"`
	}{Nodes: nodes, Relay: relay})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (c *Controller) setRuntimeTask(task *RuntimeTask) {
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	if task == nil {
		c.runtimeTask = nil
		return
	}
	copyTask := *task
	c.runtimeTask = &copyTask
}

func (c *Controller) RuntimeTask() *RuntimeTask {
	c.runtimeMu.RLock()
	defer c.runtimeMu.RUnlock()
	if c.runtimeTask == nil {
		return nil
	}
	copyTask := *c.runtimeTask
	return &copyTask
}

func (c *Controller) applyStatePath() string {
	if strings.TrimSpace(c.ManagedFile) == "" {
		return ""
	}
	return c.ManagedFile + ".apply-state.json"
}

func (c *Controller) LoadApplyState() {
	path := c.applyStatePath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var state ConfigApplyState
	if json.Unmarshal(raw, &state) != nil {
		return
	}
	c.applyMu.Lock()
	c.applyState = state
	c.applyMu.Unlock()
}

func (c *Controller) setDesiredRevision(revision string) bool {
	revision = strings.TrimSpace(revision)
	c.applyMu.Lock()
	changed := c.applyState.DesiredRevision != revision
	c.applyState.DesiredRevision = revision
	c.applyMu.Unlock()
	return changed
}

func (c *Controller) currentApplyState() ConfigApplyState {
	c.applyMu.RLock()
	state := c.applyState
	c.applyMu.RUnlock()
	return state
}

func (c *Controller) MarkConfigApply(status, message string) {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "unknown"
	}
	c.applyMu.Lock()
	c.applyState.Status = status
	c.applyState.Error = truncateStatusMessage(message, 255)
	c.applyState.UpdatedAt = time.Now().Unix()
	if status == "success" || status == "partial" {
		c.applyState.AppliedRevision = c.applyState.DesiredRevision
	}
	if status == "success" {
		c.applyState.Error = ""
	}
	state := c.applyState
	c.applyMu.Unlock()
	c.persistApplyState(state)
}

func (c *Controller) ConfigStatus() map[string]any {
	c.applyMu.RLock()
	state := c.applyState
	c.applyMu.RUnlock()
	return map[string]any{
		"config_desired_revision": state.DesiredRevision,
		"config_applied_revision": state.AppliedRevision,
		"config_apply_status":     state.Status,
		"config_apply_error":      state.Error,
		"config_apply_at":         state.UpdatedAt,
	}
}

func (c *Controller) persistApplyState(state ConfigApplyState) {
	path := c.applyStatePath()
	if path == "" {
		return
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err == nil {
		_ = atomicWrite(path, append(raw, '\n'), 0o600)
	}
}

func (c *Controller) LocalNodeCount() (int, error) {
	raw, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		return 0, err
	}
	plain, err := c.decrypt(raw)
	if err != nil {
		return 0, err
	}
	var config struct {
		Nodes []NodeConfig `json:"Nodes"`
	}
	if err := json.Unmarshal(plain, &config); err != nil {
		return 0, err
	}
	return len(config.Nodes), nil
}

func (c *Controller) LastGoodConfigPath() string {
	return c.ConfigFile + ".last-good"
}

func (c *Controller) SaveLastGoodConfig() error {
	raw, err := os.ReadFile(c.ConfigFile)
	if err != nil {
		return err
	}
	return atomicWrite(c.LastGoodConfigPath(), raw, 0o600)
}

func (c *Controller) RestoreLastGoodConfig() error {
	raw, err := os.ReadFile(c.LastGoodConfigPath())
	if err != nil {
		return err
	}
	return atomicWrite(c.ConfigFile, raw, 0o600)
}

func (c *Controller) runtimeSnapshotPath() string {
	if strings.TrimSpace(c.ManagedFile) == "" {
		return ""
	}
	return c.ManagedFile + ".runtime-cache.enc.json"
}

func (c *Controller) LoadRuntimeSnapshot() (node.RuntimeSnapshot, error) {
	var snapshot node.RuntimeSnapshot
	path := c.runtimeSnapshotPath()
	if path == "" {
		return snapshot, os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return snapshot, err
	}
	plain, err := c.decrypt(raw)
	if err != nil {
		return snapshot, err
	}
	if err := json.Unmarshal(plain, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (c *Controller) SaveRuntimeSnapshot(snapshot node.RuntimeSnapshot) error {
	path := c.runtimeSnapshotPath()
	if path == "" {
		return fmt.Errorf("runtime snapshot path is not configured")
	}
	plain, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	key, err := c.readKey()
	if err != nil {
		return err
	}
	encrypted, err := crypt.EncryptConfig(plain, key)
	if err != nil {
		return err
	}
	return atomicWrite(path, encrypted, 0o600)
}

func (c *Controller) setAutoUpdateConfig(config AutoUpdateConfig) {
	config = normalizeAutoUpdateConfig(config)
	c.updateMu.Lock()
	c.autoUpdate = config
	c.updateMu.Unlock()
}

func (c *Controller) AutoUpdateConfig() AutoUpdateConfig {
	c.updateMu.RLock()
	defer c.updateMu.RUnlock()
	return c.autoUpdate
}

func (c *Controller) AcknowledgeUpdate(config AutoUpdateConfig, status, installedVersion, message string) error {
	var response map[string]any
	return c.Client.Post(updateAckPath, map[string]string{
		"request_id":        config.RequestID,
		"status":            status,
		"target_version":    config.TargetVersion,
		"installed_version": installedVersion,
		"error":             truncateStatusMessage(message, 255),
	}, &response)
}

func (c *Controller) ClearUpdateRequest(requestID string) {
	c.updateMu.Lock()
	if c.autoUpdate.RequestID == requestID {
		c.autoUpdate.RequestID = ""
		c.autoUpdate.TargetVersion = ""
		c.autoUpdate.RequestedAt = 0
	}
	c.updateMu.Unlock()
}

func truncateStatusMessage(message string, limit int) string {
	message = strings.TrimSpace(message)
	runes := []rune(message)
	if len(runes) <= limit {
		return message
	}
	return string(runes[:limit])
}

func (c *Controller) setNetworkQualityConfig(config NetworkQualityConfig) {
	config = normalizeNetworkQualityConfig(config)
	c.qualityMu.Lock()
	c.quality = config
	c.qualityMu.Unlock()
}

func (c *Controller) NetworkQualityConfig() NetworkQualityConfig {
	c.qualityMu.RLock()
	defer c.qualityMu.RUnlock()
	config := c.quality
	config.Targets = append([]NetworkQualityTarget(nil), config.Targets...)
	return config
}

func (c *Controller) PushStatus(status map[string]any) error {
	if c.Relay != nil {
		for key, value := range c.Relay.Status() {
			status[key] = value
		}
	}
	for key, value := range c.ConfigStatus() {
		status[key] = value
	}
	var response map[string]any
	return c.Client.Post(statusPath, status, &response)
}

func (c *Controller) writeMergedConfig(desired []NodeConfig) (bool, error) {
	existing := map[string]json.RawMessage{}
	var existingNodes []NodeConfig
	if raw, err := os.ReadFile(c.ConfigFile); err == nil {
		plain, decodeErr := c.decrypt(raw)
		if decodeErr != nil {
			return false, decodeErr
		}
		if err := json.Unmarshal(plain, &existing); err != nil {
			return false, fmt.Errorf("decode existing config: %w", err)
		}
		if nodeRaw := existing["Nodes"]; len(nodeRaw) > 0 {
			_ = json.Unmarshal(nodeRaw, &existingNodes)
		}
	}

	previousManaged := map[string]bool{}
	if raw, err := os.ReadFile(c.ManagedFile); err == nil {
		var identities []string
		if json.Unmarshal(raw, &identities) == nil {
			for _, identity := range identities {
				previousManaged[identity] = true
			}
		}
	}

	manual := make([]NodeConfig, 0, len(existingNodes))
	seen := map[string]bool{}
	for _, node := range existingNodes {
		identity := nodeIdentity(node)
		if identity == "" || previousManaged[identity] || seen[identity] {
			continue
		}
		seen[identity] = true
		manual = append(manual, node)
	}
	merged := append([]NodeConfig{}, manual...)
	managedIdentities := make([]string, 0, len(desired))
	for _, node := range desired {
		identity := nodeIdentity(node)
		if identity == "" || seen[identity] {
			continue
		}
		if node.Timeout <= 0 {
			node.Timeout = 15
		}
		seen[identity] = true
		merged = append(merged, node)
		managedIdentities = append(managedIdentities, identity)
	}

	if _, ok := existing["Log"]; !ok {
		existing["Log"] = json.RawMessage(`{"Level":"warning","Output":"","Access":"none"}`)
	}
	nodeRaw, err := json.Marshal(merged)
	if err != nil {
		return false, err
	}
	existing["Nodes"] = nodeRaw
	plain, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return false, err
	}

	changed := true
	if current, err := os.ReadFile(c.ConfigFile); err == nil {
		if currentPlain, err := c.decrypt(current); err == nil {
			changed = !jsonEqual(currentPlain, plain)
		}
	}
	if changed {
		key, err := c.readKey()
		if err != nil {
			return false, err
		}
		encrypted, err := crypt.EncryptConfig(plain, key)
		if err != nil {
			return false, err
		}
		if err := atomicWrite(c.ConfigFile, encrypted, 0o600); err != nil {
			return false, err
		}
	}
	managedRaw, _ := json.MarshalIndent(managedIdentities, "", "  ")
	managedChanged := true
	if current, err := os.ReadFile(c.ManagedFile); err == nil {
		managedChanged = !jsonEqual(current, managedRaw)
	}
	if managedChanged {
		if err := atomicWrite(c.ManagedFile, managedRaw, 0o600); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (c *Controller) readKey() ([]byte, error) {
	if key, err := crypt.ReadConfigKeyFromEnv(); err == nil {
		return key, nil
	}
	raw, err := os.ReadFile(c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read config key: %w", err)
	}
	return crypt.ParseConfigKey(string(raw))
}

func (c *Controller) decrypt(raw []byte) ([]byte, error) {
	var envelope crypt.ConfigEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.Ciphertext == "" {
		return raw, nil
	}
	key, err := c.readKey()
	if err != nil {
		return nil, err
	}
	return crypt.DecryptConfig(raw, key)
}

func nodeIdentity(node NodeConfig) string {
	host := strings.TrimRight(strings.ToLower(strings.TrimSpace(node.APIHost)), "/")
	if node.NodeID > 0 {
		return strconv.Itoa(node.NodeID)
	}
	if host == "" {
		return ""
	}
	return host + "#" + strconv.Itoa(node.NodeID)
}

func jsonEqual(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && reflect.DeepEqual(left, right)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ravel-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmpName, path)
}
