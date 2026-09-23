package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/domaintraffic"
	"time"
)

func (c *Controller) reportDomainTraffic(ctx context.Context) {
	if c.info.Common.BaseConfig == nil || !c.info.Common.BaseConfig.DomainTrafficEnable {
		domaintraffic.Enable(c.tag, false)
		c.pendingDomains = nil
		return
	}
	if time.Since(c.lastDomainReport) < 5*time.Minute {
		return
	}
	c.lastDomainReport = time.Now()
	if c.pendingDomains == nil {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return
		}
		records := domaintraffic.DrainLimited(c.tag, 15*1024)
		if len(records) == 0 {
			return
		}
		c.pendingDomains = &panel.DomainTrafficBatch{ID: hex.EncodeToString(id[:]), RecordedAt: time.Now().Unix(), Records: records}
	}
	if err := c.apiClient.ReportDomainTraffic(ctx, c.pendingDomains); err != nil {
		log.WithError(err).WithField("tag", c.tag).Warn("Domain traffic report failed")
		return
	}
	c.pendingDomains = nil
}
