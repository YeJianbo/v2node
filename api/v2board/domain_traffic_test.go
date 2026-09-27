package panel

import (
	"context"
	"github.com/go-resty/resty/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDomainReportDoesNotUseGeneralRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(503)
	}))
	defer server.Close()
	client := resty.New().SetBaseURL(server.URL).SetRetryCount(3)
	client.AddRetryCondition(func(r *resty.Response, err error) bool { return true })
	c := &Client{client: client}
	if c.ReportDomainTraffic(context.Background(), &DomainTrafficBatch{}) == nil {
		t.Fatal("failure accepted")
	}
	if attempts != 1 {
		t.Fatalf("retried %d times", attempts)
	}
	if client.RetryCount != 3 {
		t.Fatal("changed other report retries")
	}
}
