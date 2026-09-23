package panel

import (
	"context"
	"fmt"
	"github.com/wyx2685/v2node/common/domaintraffic"
	"time"
)

type DomainTrafficBatch struct {
	ID         string                 `json:"batch_id"`
	RecordedAt int64                  `json:"recorded_at"`
	Records    []domaintraffic.Record `json:"records"`
}

func (c *Client) ReportDomainTraffic(ctx context.Context, batch *DomainTrafficBatch) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The controller retains the batch and retries on the next reporting window.
	r, err := c.client.Clone().SetRetryCount(0).R().SetContext(ctx).SetBody(batch).Post("/api/v2/server/domainTraffic")
	if err != nil {
		return fmt.Errorf("domain traffic transport failed")
	}
	if r == nil || r.StatusCode() != 200 || !r.IsSuccess() {
		return fmt.Errorf("domain traffic report rejected")
	}
	// Authentication failures in legacy panels can use HTTP 200.
	if !jsonAccepted(r.Body()) {
		return fmt.Errorf("domain traffic report not acknowledged")
	}
	return nil
}
