package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"encoding/json"

	ravel "github.com/YeJianbo/ravel-go"
	"github.com/vmihailenco/msgpack/v5"
)

type OnlineUser struct {
	UID int
	IP  string
}

type UserInfo struct {
	Id               int               `json:"id" msgpack:"id"`
	Uuid             string            `json:"uuid" msgpack:"uuid"`
	SpeedLimit       int               `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit      int               `json:"device_limit" msgpack:"device_limit"`
	RavelCredentials []RavelCredential `json:"ravel_credentials,omitempty" msgpack:"ravel_credentials,omitempty"`
}

type RavelCredential struct {
	CredentialID  string `json:"credential_id" msgpack:"credential_id"`
	CapabilityKey string `json:"capability_key" msgpack:"capability_key"`
	KeyVersion    uint32 `json:"key_version" msgpack:"key_version"`
	PolicyID      uint32 `json:"policy_id" msgpack:"policy_id"`
	GatewayGroup  string `json:"gateway_group" msgpack:"gateway_group"`
	NotBefore     int64  `json:"not_before" msgpack:"not_before"`
	NotAfter      int64  `json:"not_after" msgpack:"not_after"`
}

type UserListBody struct {
	Users []UserInfo `json:"users" msgpack:"users"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

// GetUserList will pull user from v2board
func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	const path = "/api/v1/server/UniProxy/user"
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(path)
	if err != nil {
		return nil, c.sanitizeRequestError(err)
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer r.RawResponse.Body.Close()

	if r.StatusCode() == 304 {
		return nil, nil
	}
	if r.StatusCode() < 200 || r.StatusCode() >= 300 {
		return nil, fmt.Errorf("get user list returned HTTP %d", r.StatusCode())
	}
	body, err := io.ReadAll(r.RawResponse.Body)
	if err != nil {
		return nil, fmt.Errorf("read user list error: %w", err)
	}
	userlist := &UserListBody{}
	if strings.Contains(r.Header().Get("Content-Type"), "application/x-msgpack") {
		reader := bytes.NewReader(body)
		decoder := msgpack.NewDecoder(reader)
		if err := decoder.Decode(userlist); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
		if reader.Len() != 0 {
			return nil, fmt.Errorf("decode user list error: trailing msgpack data")
		}
	} else {
		if err := json.Unmarshal(body, userlist); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	}
	if userlist.Users == nil {
		return nil, errors.New("user response is missing the users array")
	}
	if err := validateRavelCredentials(userlist.Users); err != nil {
		return nil, err
	}

	// Commit the ETag only after the complete response has decoded and every
	// credential has passed semantic validation.
	c.userEtag = r.Header().Get("ETag")
	return userlist.Users, nil
}

// A valid response can still fail to apply to the runtime. Fetch it again on
// the next poll instead of allowing a 304 to hide the unapplied snapshot.
func (c *Client) InvalidateUserCache() { c.userEtag = "" }

func validateRavelCredentials(users []UserInfo) error {
	for userIndex := range users {
		for credentialIndex := range users[userIndex].RavelCredentials {
			if err := users[userIndex].RavelCredentials[credentialIndex].validate(); err != nil {
				return fmt.Errorf(
					"invalid ravel credential for user %d at index %d: %w",
					users[userIndex].Id,
					credentialIndex,
					err,
				)
			}
		}
	}
	return nil
}

func (credential RavelCredential) validate() error {
	if credential.PolicyID != 0 {
		return errors.New("unsupported ravel policy_id")
	}
	if credential.NotBefore < 0 || credential.NotAfter <= credential.NotBefore {
		return errors.New("invalid ravel credential validity interval")
	}
	if _, err := ravel.ParseCredential(
		credential.CredentialID,
		credential.CapabilityKey,
		credential.KeyVersion,
		credential.PolicyID,
		credential.GatewayGroup,
		credential.NotBefore,
		credential.NotAfter,
	); err != nil {
		return err
	}
	return nil
}

// GetUserAlive will fetch the alive_ip count for users
func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	c.AliveMap = &AliveMap{}
	const path = "/api/v1/server/UniProxy/alivelist"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, c.sanitizeRequestError(err)
		}
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	if r == nil || r.RawResponse == nil || r.StatusCode() >= 399 {
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	defer r.RawResponse.Body.Close()
	if err := json.Unmarshal(r.Body(), c.AliveMap); err != nil {
		fmt.Printf("unmarshal user alive list error: %s", err)
		c.AliveMap.Alive = make(map[int]int)
	}

	return c.AliveMap.Alive, nil
}

type UserTraffic struct {
	UID      int
	Upload   int64
	Download int64
}

// ReportUserTraffic reports the user traffic
func (c *Client) ReportUserTraffic(ctx context.Context, userTraffic []UserTraffic) error {
	data := make(map[int][]int64, len(userTraffic))
	for i := range userTraffic {
		data[userTraffic[i].UID] = []int64{userTraffic[i].Upload, userTraffic[i].Download}
	}
	const path = "/api/v1/server/UniProxy/push"
	response, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return c.sanitizeRequestError(err)
	}
	if response == nil || response.StatusCode() < 200 || response.StatusCode() >= 300 {
		return fmt.Errorf("traffic report was not accepted by panel")
	}
	return nil
}

func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data *map[int][]string) error {
	const path = "/api/v1/server/UniProxy/alive"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)

	if err != nil {
		return c.sanitizeRequestError(err)
	}

	return nil
}
