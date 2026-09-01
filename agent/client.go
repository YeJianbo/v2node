package agent

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const protocol = "ravel-v1"

type State struct {
	PanelURL       string `json:"panel_url"`
	MachineToken   string `json:"machine_token"`
	MachineID      int    `json:"machine_id"`
	Protocol       string `json:"protocol"`
	SyncInterval   int    `json:"sync_interval"`
	StatusInterval int    `json:"status_interval"`
}

type Client struct {
	baseURL   string
	token     string
	machineID int
	http      *http.Client
}

func NewClient(state State) (*Client, error) {
	if strings.TrimSpace(state.PanelURL) == "" || strings.TrimSpace(state.MachineToken) == "" || state.MachineID <= 0 {
		return nil, fmt.Errorf("incomplete ravel state")
	}
	if state.Protocol != "" && state.Protocol != protocol {
		return nil, fmt.Errorf("unsupported protocol %q", state.Protocol)
	}
	return &Client{
		baseURL:   strings.TrimRight(state.PanelURL, "/"),
		token:     state.MachineToken,
		machineID: state.MachineID,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
			},
		},
	}, nil
}

func (c *Client) Get(path string, query url.Values, out any) error {
	return c.do(http.MethodGet, path, query, nil, out)
}

func (c *Client) Post(path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, path, nil, raw, out)
}

func (c *Client) do(method, path string, query url.Values, body []byte, out any) error {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	rawQuery := ""
	if query != nil {
		rawQuery = query.Encode()
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	bodyDigest := sha256.Sum256(body)
	payload := strings.Join([]string{method, path, rawQuery, timestamp, nonce, hex.EncodeToString(bodyDigest[:])}, "\n")
	mac := hmac.New(sha256.New, []byte(c.token))
	_, _ = mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))

	requestURL := c.baseURL + path
	if rawQuery != "" {
		requestURL += "?" + rawQuery
	}
	req, err := http.NewRequest(method, requestURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "ravel")
	req.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Ravel-Machine-Id", strconv.Itoa(c.machineID))
	req.Header.Set("X-Ravel-Timestamp", timestamp)
	req.Header.Set("X-Ravel-Nonce", nonce)
	req.Header.Set("X-Ravel-Signature", signature)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("panel returned HTTP %d", resp.StatusCode)
	}
	if out != nil && len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, out); err != nil {
			return fmt.Errorf("decode panel response: %w", err)
		}
	}
	return nil
}
