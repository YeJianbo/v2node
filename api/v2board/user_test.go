package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
	"github.com/vmihailenco/msgpack/v5"
)

func TestGetUserListParsesRavelCredentialsJSONAndMsgpack(t *testing.T) {
	payload := UserListBody{Users: []UserInfo{{
		Id:               42,
		Uuid:             "user-42",
		SpeedLimit:       100,
		DeviceLimit:      2,
		RavelCredentials: []RavelCredential{validRavelCredential()},
	}}}
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	msgpackBody, err := msgpack.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		contentType string
		body        []byte
	}{
		{name: "json", contentType: "application/json", body: jsonBody},
		{name: "msgpack", contentType: "application/x-msgpack", body: msgpackBody},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("ETag", `"users-v1"`)
				_, _ = w.Write(test.body)
			}))
			defer server.Close()

			client := &Client{client: resty.New().SetBaseURL(server.URL)}
			users, err := client.GetUserList(context.Background())
			if err != nil {
				t.Fatalf("GetUserList() error = %v", err)
			}
			if len(users) != 1 || len(users[0].RavelCredentials) != 1 {
				t.Fatalf("unexpected users: %+v", users)
			}
			if users[0].RavelCredentials[0] != payload.Users[0].RavelCredentials[0] {
				t.Fatalf("credential mismatch: %+v", users[0].RavelCredentials[0])
			}
			if client.userEtag != `"users-v1"` {
				t.Fatalf("userEtag = %q, want users-v1", client.userEtag)
			}
		})
	}
}

func TestGetUserListInvalidCredentialDoesNotCommitETagOrRevealKey(t *testing.T) {
	validPayload := UserListBody{Users: []UserInfo{{
		Id:               7,
		Uuid:             "user-7",
		RavelCredentials: []RavelCredential{validRavelCredential()},
	}}}
	validBody, err := json.Marshal(validPayload)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("a", 58) + "SECRET"
	invalidPayload := validPayload
	invalidPayload.Users = append([]UserInfo(nil), validPayload.Users...)
	invalidPayload.Users[0].RavelCredentials = append(
		[]RavelCredential(nil),
		validPayload.Users[0].RavelCredentials...,
	)
	invalidPayload.Users[0].RavelCredentials[0].CapabilityKey = secret
	invalidBody, err := json.Marshal(invalidPayload)
	if err != nil {
		t.Fatal(err)
	}

	requestCount := 0
	var secondIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			w.Header().Set("ETag", `"good-users"`)
			_, _ = w.Write(validBody)
			return
		}
		secondIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"bad-users"`)
		_, _ = w.Write(invalidBody)
	}))
	defer server.Close()

	client := &Client{client: resty.New().SetBaseURL(server.URL)}
	if _, err := client.GetUserList(context.Background()); err != nil {
		t.Fatalf("first GetUserList() error = %v", err)
	}
	_, err = client.GetUserList(context.Background())
	if err == nil {
		t.Fatal("second GetUserList() accepted invalid credential")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error revealed capability key: %v", err)
	}
	if secondIfNoneMatch != `"good-users"` {
		t.Fatalf("If-None-Match = %q, want previous valid ETag", secondIfNoneMatch)
	}
	if client.userEtag != `"good-users"` {
		t.Fatalf("invalid credential committed ETag %q", client.userEtag)
	}
}

func TestRavelCredentialValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RavelCredential)
	}{
		{
			name: "credential id length",
			mutate: func(credential *RavelCredential) {
				credential.CredentialID = "abcd"
			},
		},
		{
			name: "credential id hex",
			mutate: func(credential *RavelCredential) {
				credential.CredentialID = strings.Repeat("z", 32)
			},
		},
		{
			name: "capability key hex",
			mutate: func(credential *RavelCredential) {
				credential.CapabilityKey = strings.Repeat("z", 64)
			},
		},
		{
			name: "gateway group",
			mutate: func(credential *RavelCredential) {
				credential.GatewayGroup = "short"
			},
		},
		{
			name: "policy",
			mutate: func(credential *RavelCredential) {
				credential.PolicyID = 1
			},
		},
		{
			name: "negative time",
			mutate: func(credential *RavelCredential) {
				credential.NotBefore = -1
			},
		},
		{
			name: "reversed time",
			mutate: func(credential *RavelCredential) {
				credential.NotAfter = credential.NotBefore
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential := validRavelCredential()
			test.mutate(&credential)
			if err := credential.validate(); err == nil {
				t.Fatal("validate() accepted invalid credential")
			}
		})
	}
}

func validRavelCredential() RavelCredential {
	return RavelCredential{
		CredentialID:  strings.Repeat("a", 32),
		CapabilityKey: strings.Repeat("b", 64),
		KeyVersion:    2,
		PolicyID:      0,
		GatewayGroup:  "gw000001",
		NotBefore:     100,
		NotAfter:      200,
	}
}
