package agent

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/wyx2685/v2node/common/crypt"
)

const configPath = "/api/v1/server/machine/v2nodeConfig"
const statusPath = "/api/v1/server/machine/push"
const restartAckPath = "/api/v1/server/machine/restartAck"

type NodeConfig struct {
	APIHost string `json:"ApiHost"`
	NodeID  int    `json:"NodeID"`
	APIKey  string `json:"ApiKey"`
	Timeout int    `json:"Timeout"`
}

type ConfigResponse struct {
	Data               []NodeConfig `json:"data"`
	RestartToken       string       `json:"restart_node_token"`
	LegacyRestartToken string       `json:"restart_v2node_token"`
	Probe              ProbeConfig  `json:"probe"`
}

type ProbeConfig struct {
	Relay RelayConfig `json:"relay"`
}

type Controller struct {
	Client      *Client
	ConfigFile  string
	KeyFile     string
	ManagedFile string
	Relay       *RelayManager
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
	if c.Relay != nil {
		_, _ = c.Relay.Sync(response.Probe.Relay)
	}
	changed, err := c.writeMergedConfig(response.Data)
	if err != nil {
		return false, 0, err
	}
	restartToken := response.RestartToken
	if restartToken == "" {
		restartToken = response.LegacyRestartToken
	}
	restartRequested := restartToken != ""
	if restartRequested {
		var ack map[string]any
		if err := c.Client.Post(restartAckPath, map[string]string{"restart_token": restartToken}, &ack); err != nil {
			return false, len(response.Data), fmt.Errorf("acknowledge restart: %w", err)
		}
	}
	return changed || restartRequested, len(response.Data), nil
}

func (c *Controller) PushStatus(status map[string]any) error {
	if c.Relay != nil {
		for key, value := range c.Relay.Status() {
			status[key] = value
		}
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
	if err := atomicWrite(c.ManagedFile, managedRaw, 0o600); err != nil {
		return false, err
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
