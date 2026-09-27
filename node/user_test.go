package node

import (
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestCompareUserListIgnoresRavelCredentialOrder(t *testing.T) {
	first := testRavelCredential("a", "b", 1, 200)
	second := testRavelCredential("c", "d", 2, 300)
	oldUsers := []panel.UserInfo{{
		Uuid:             "user-1",
		RavelCredentials: []panel.RavelCredential{first, second},
	}}
	newUsers := []panel.UserInfo{{
		Uuid:             "user-1",
		RavelCredentials: []panel.RavelCredential{second, first},
	}}

	deleted, added, modified := compareUserList(oldUsers, newUsers)
	if len(deleted) != 0 || len(added) != 0 || len(modified) != 0 {
		t.Fatalf("reordering changed users: deleted=%d added=%d modified=%d", len(deleted), len(added), len(modified))
	}
	if ravelCredentialFingerprint(oldUsers[0].RavelCredentials) !=
		ravelCredentialFingerprint(newUsers[0].RavelCredentials) {
		t.Fatal("credential fingerprint is order-dependent")
	}
}

func TestCompareUserListDetectsRavelCredentialFieldChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*panel.RavelCredential)
	}{
		{
			name: "credential id",
			mutate: func(credential *panel.RavelCredential) {
				credential.CredentialID = strings.Repeat("e", 32)
			},
		},
		{
			name: "capability key",
			mutate: func(credential *panel.RavelCredential) {
				credential.CapabilityKey = strings.Repeat("f", 64)
			},
		},
		{
			name: "key version",
			mutate: func(credential *panel.RavelCredential) {
				credential.KeyVersion++
			},
		},
		{
			name: "policy",
			mutate: func(credential *panel.RavelCredential) {
				credential.PolicyID++
			},
		},
		{
			name: "gateway group",
			mutate: func(credential *panel.RavelCredential) {
				credential.GatewayGroup = "gw000002"
			},
		},
		{
			name: "not before",
			mutate: func(credential *panel.RavelCredential) {
				credential.NotBefore++
			},
		},
		{
			name: "expiry",
			mutate: func(credential *panel.RavelCredential) {
				credential.NotAfter++
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldCredential := testRavelCredential("a", "b", 1, 200)
			newCredential := oldCredential
			test.mutate(&newCredential)
			oldUsers := []panel.UserInfo{{
				Uuid:             "user-1",
				RavelCredentials: []panel.RavelCredential{oldCredential},
			}}
			newUsers := []panel.UserInfo{{
				Uuid:             "user-1",
				RavelCredentials: []panel.RavelCredential{newCredential},
			}}

			deleted, added, modified := compareUserList(oldUsers, newUsers)
			if len(deleted) != 0 || len(added) != 0 || len(modified) != 1 {
				t.Fatalf("field change not detected: deleted=%d added=%d modified=%d", len(deleted), len(added), len(modified))
			}
		})
	}
}

func TestCompareUserListDetectsRotationAndRevocation(t *testing.T) {
	first := testRavelCredential("a", "b", 1, 200)
	second := testRavelCredential("c", "d", 2, 300)
	base := panel.UserInfo{
		Uuid:             "user-1",
		RavelCredentials: []panel.RavelCredential{first},
	}

	t.Run("rotation overlap adds credential", func(t *testing.T) {
		rotated := base
		rotated.RavelCredentials = []panel.RavelCredential{first, second}
		_, _, modified := compareUserList([]panel.UserInfo{base}, []panel.UserInfo{rotated})
		if len(modified) != 1 {
			t.Fatalf("rotation overlap modified=%d, want 1", len(modified))
		}
	})

	t.Run("revocation removes credential", func(t *testing.T) {
		rotated := base
		rotated.RavelCredentials = []panel.RavelCredential{first, second}
		revoked := base
		revoked.RavelCredentials = []panel.RavelCredential{second}
		_, _, modified := compareUserList([]panel.UserInfo{rotated}, []panel.UserInfo{revoked})
		if len(modified) != 1 {
			t.Fatalf("revocation modified=%d, want 1", len(modified))
		}
	})
}

func TestCompareUserListNonRavelRegression(t *testing.T) {
	oldUsers := []panel.UserInfo{{
		Uuid:        "user-1",
		SpeedLimit:  100,
		DeviceLimit: 2,
	}}
	sameUsers := append([]panel.UserInfo(nil), oldUsers...)
	deleted, added, modified := compareUserList(oldUsers, sameUsers)
	if len(deleted) != 0 || len(added) != 0 || len(modified) != 0 {
		t.Fatalf("unchanged non-Ravel user changed: %d/%d/%d", len(deleted), len(added), len(modified))
	}

	changedUsers := append([]panel.UserInfo(nil), oldUsers...)
	changedUsers[0].SpeedLimit++
	_, _, modified = compareUserList(oldUsers, changedUsers)
	if len(modified) != 1 {
		t.Fatalf("speed limit change modified=%d, want 1", len(modified))
	}
}

func testRavelCredential(idDigit, keyDigit string, version uint32, notAfter int64) panel.RavelCredential {
	return panel.RavelCredential{
		CredentialID:  strings.Repeat(idDigit, 32),
		CapabilityKey: strings.Repeat(keyDigit, 64),
		KeyVersion:    version,
		PolicyID:      0,
		GatewayGroup:  "gw000001",
		NotBefore:     100,
		NotAfter:      notAfter,
	}
}
