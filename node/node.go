package node

import (
	"context"
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

type Node struct {
	controllers []*Controller
	NodeInfos   []*panel.NodeInfo
	configs     []conf.NodeConfig
	failures    []NodeFailure
	requested   int
}

type NodeFailure struct {
	APIHost string `json:"api_host"`
	NodeID  int    `json:"node_id"`
	Stage   string `json:"stage"`
	Error   string `json:"error"`
}

type RuntimeSnapshot struct {
	Nodes []CachedNode `json:"nodes"`
}

type CachedNode struct {
	APIHost string           `json:"api_host"`
	NodeID  int              `json:"node_id"`
	Info    *panel.NodeInfo  `json:"info"`
	Users   []panel.UserInfo `json:"users"`
	Alive   map[int]int      `json:"alive"`
}

func New(nodes []conf.NodeConfig) (*Node, error) {
	return NewWithSnapshot(nodes, RuntimeSnapshot{})
}

func NewWithSnapshot(nodes []conf.NodeConfig, snapshot RuntimeSnapshot) (*Node, error) {
	n := &Node{
		controllers: make([]*Controller, 0, len(nodes)),
		NodeInfos:   make([]*panel.NodeInfo, 0, len(nodes)),
		configs:     make([]conf.NodeConfig, 0, len(nodes)),
		failures:    make([]NodeFailure, 0),
		requested:   len(nodes),
	}
	cached := make(map[string]CachedNode, len(snapshot.Nodes))
	for _, item := range snapshot.Nodes {
		cached[nodeConfigIdentity(item.APIHost, item.NodeID)] = item
	}
	for _, nodeConfig := range nodes {
		p, err := panel.New(&nodeConfig)
		if err != nil {
			n.addFailure(nodeConfig, "initialize", err)
			continue
		}
		info, err := p.GetNodeInfo(context.Background())
		if err != nil {
			item, ok := cached[nodeConfigIdentity(nodeConfig.APIHost, nodeConfig.NodeID)]
			if !ok || item.Info == nil {
				n.addFailure(nodeConfig, "fetch", err)
				continue
			}
			info = item.Info
			log.WithError(err).Warnf("using cached node runtime data for %s#%d", nodeConfig.APIHost, nodeConfig.NodeID)
		}
		controller := NewController(p, &nodeConfig, info)
		if item, ok := cached[nodeConfigIdentity(nodeConfig.APIHost, nodeConfig.NodeID)]; ok {
			controller.SetCachedRuntime(item.Users, item.Alive)
		}
		n.controllers = append(n.controllers, controller)
		n.NodeInfos = append(n.NodeInfos, info)
		n.configs = append(n.configs, nodeConfig)
	}
	return n, nil
}

func (n *Node) Start(_ []conf.NodeConfig, core *core.V2Core) error {
	active := make([]*Controller, 0, len(n.controllers))
	for i, controller := range n.controllers {
		err := controller.Start(core)
		if err != nil {
			n.addFailure(n.configs[i], "start", err)
			continue
		}
		active = append(active, controller)
	}
	n.controllers = active
	if len(n.failures) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d managed nodes failed: %s", len(n.failures), n.requested, n.FailureMessage())
}

func (n *Node) Close() error {
	var result error
	for _, c := range n.controllers {
		if err := c.Close(); err != nil {
			log.Errorf("close controller failed: %v", err)
			result = errors.Join(result, err)
		}
	}
	n.controllers = nil
	return result
}

func (n *Node) ActiveCount() int {
	return len(n.controllers)
}

func (n *Node) RequestedCount() int {
	return n.requested
}

func (n *Node) Failures() []NodeFailure {
	return append([]NodeFailure(nil), n.failures...)
}

func (n *Node) RuntimeSnapshot() RuntimeSnapshot {
	snapshot := RuntimeSnapshot{Nodes: make([]CachedNode, 0, len(n.controllers))}
	for _, controller := range n.controllers {
		snapshot.Nodes = append(snapshot.Nodes, controller.RuntimeSnapshot())
	}
	return snapshot
}

func (n *Node) FailureMessage() string {
	messages := make([]string, 0, len(n.failures))
	for _, failure := range n.failures {
		messages = append(messages, fmt.Sprintf("%s#%d %s: %s", failure.APIHost, failure.NodeID, failure.Stage, failure.Error))
	}
	return strings.Join(messages, "; ")
}

func (n *Node) addFailure(config conf.NodeConfig, stage string, err error) {
	n.failures = append(n.failures, NodeFailure{
		APIHost: config.APIHost,
		NodeID:  config.NodeID,
		Stage:   stage,
		Error:   err.Error(),
	})
}

func nodeConfigIdentity(apiHost string, nodeID int) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(apiHost)), "/") + "#" + fmt.Sprint(nodeID)
}
