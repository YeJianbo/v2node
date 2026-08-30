package node

import (
	"context"
	"errors"
	"fmt"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
	"github.com/wyx2685/v2node/limiter"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	aliveMap                map[int]int
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	renewCertPeriodic       *task.Task
	hasCachedRuntime        bool
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, info *panel.NodeInfo) *Controller {
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      conf,
	}
	return controller
}

func (c *Controller) SetCachedRuntime(users []panel.UserInfo, alive map[int]int) {
	if len(users) == 0 {
		return
	}
	c.userList = append([]panel.UserInfo(nil), users...)
	c.aliveMap = make(map[int]int, len(alive))
	for userID, count := range alive {
		c.aliveMap[userID] = count
	}
	c.hasCachedRuntime = true
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	var err error
	// First fetch Node Info
	node := c.info
	if node == nil {
		c.info, err = c.apiClient.GetNodeInfo(context.Background())
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		node = c.info
	}
	// Update user
	users, userErr := c.apiClient.GetUserList(context.Background())
	if userErr == nil {
		if len(users) == 0 {
			return errors.New("add users error: not have any user")
		}
		c.userList = users
		c.hasCachedRuntime = false
	} else if !c.hasCachedRuntime {
		return fmt.Errorf("get user list error: %s", userErr)
	} else {
		log.WithError(userErr).Warnf("using cached users for %s", c.conf.APIHost)
	}
	if len(c.userList) == 0 {
		return errors.New("add users error: not have any user")
	}
	alive, aliveErr := c.apiClient.GetUserAlive(context.Background())
	if aliveErr == nil {
		c.aliveMap = alive
	} else if c.aliveMap == nil {
		return fmt.Errorf("failed to get user alive list: %s", aliveErr)
	} else {
		log.WithError(aliveErr).Warnf("using cached user limits for %s", c.conf.APIHost)
	}
	c.tag = node.Tag

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap)
	c.limiter = l
	if node.Security == panel.Tls {
		err = c.requestCert()
		if err != nil {
			limiter.DeleteLimiter(c.tag)
			c.limiter = nil
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	// Add new tag
	err = c.server.AddNode(c.tag, node)
	if err != nil {
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
		return fmt.Errorf("add new node error: %s", err)
	}
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		_ = c.server.DelNode(c.tag)
		limiter.DeleteLimiter(c.tag)
		c.limiter = nil
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	c.startTasks(node)
	return nil
}

func (c *Controller) RuntimeSnapshot() CachedNode {
	alive := make(map[int]int, len(c.aliveMap))
	for userID, count := range c.aliveMap {
		alive[userID] = count
	}
	return CachedNode{
		APIHost: c.conf.APIHost,
		NodeID:  c.conf.NodeID,
		Info:    c.info,
		Users:   append([]panel.UserInfo(nil), c.userList...),
		Alive:   alive,
	}
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	if c.tag != "" {
		limiter.DeleteLimiter(c.tag)
	}
	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	if c.server == nil || c.tag == "" {
		return nil
	}
	err := c.server.DelNode(c.tag)
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}
