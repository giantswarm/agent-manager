package kagenttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// Grant is a workspace grant's payload beside the registered claims, as a
// workspace manager signs it and the controller's jwt admission reads it.
type Grant struct {
	Workspace struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"workspace"`
	Volume struct {
		Driver string `json:"driver"`
		Handle string `json:"handle"`
	} `json:"volume"`
	Mounts []GrantMount `json:"mounts"`
}

// GrantMount is one directory of the volume a grant allows a session to mount.
type GrantMount struct {
	SubPath  string `json:"sub_path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// WorkspaceGrant is a grant for the workspace kagent/platform on an NFS
// volume: the session's own directory read-write and the mirrors read-only,
// what a workspace manager issues.
func WorkspaceGrant() Grant {
	var g Grant
	g.Workspace.Namespace, g.Workspace.Name = "kagent", "platform"
	g.Volume.Driver, g.Volume.Handle = "nfs.csi.k8s.io", "nfs-server.agentlab.svc.cluster.local#export#workspace-platform#"
	g.Mounts = []GrantMount{{SubPath: "sessions/${SESSION_ID}"}, {SubPath: "mirrors", ReadOnly: true}}
	return g
}

// Signer signs grants as a workspace manager does, with a key of its own.
type Signer struct {
	key *ecdsa.PrivateKey
	// Now is the signing clock; default time.Now.
	Now func() time.Time
}

// NewSigner makes a Signer with a fresh ES256 key.
func NewSigner(t *testing.T) *Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return &Signer{key: key}
}

// Sign answers the compact grant for subject, valid for ttl from now (a
// negative ttl is an expired grant), with the registered claims a workspace
// manager sets.
func (s *Signer) Sign(t *testing.T, subject string, ttl time.Duration, grant Grant) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: s.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	require.NoError(t, err)
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	issued := now().UTC().Truncate(time.Second)
	claims := jwt.Claims{
		Issuer:    "https://workspaces.lab.local",
		Subject:   subject,
		Audience:  jwt.Audience{"kagent"},
		IssuedAt:  jwt.NewNumericDate(issued),
		NotBefore: jwt.NewNumericDate(issued),
		Expiry:    jwt.NewNumericDate(issued.Add(ttl)),
		ID:        "grant-1",
	}
	token, err := jwt.Signed(signer).Claims(claims).Claims(grant).Serialize()
	require.NoError(t, err)
	return token
}
