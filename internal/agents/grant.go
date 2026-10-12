package agents

import (
	"context"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/giantswarm/agent-manager/internal/identity"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
)

// A workspace grant is the short-lived JWT a workspace manager's
// authorize_workspace returns once the caller may read every repository of a
// workspace: it names the person (sub, their email, as kagent names a
// Session's creator), the workspace, its volume and the directories of it a
// Session may mount. agent-manager holds no key and verifies nothing: it
// decodes the grant to refuse at once, with the reason, what kagent's
// admission would refuse anyway (another person's, an expired or a malformed
// grant) and to compose the Session's volume source from it, and passes the
// grant on as the request's admission token, which kagent verifies against
// the workspace manager's published keys. The grant appears in no log line,
// error or response.

const (
	// MaxWorkspaceGrantLength is kagent's bound on an admission token.
	MaxWorkspaceGrantLength = 16384
	// WorkspaceMountPath is where kagent mounts the Session's own directory
	// of the workspace volume, read-write: the working directory of its turns.
	WorkspaceMountPath = "/workspace"
	// MirrorsMountPath is where kagent mounts the volume's shared directory,
	// read-only: the git mirrors the Session's clones borrow objects from.
	MirrorsMountPath = "/mirrors"
)

// grantAlgorithms are the signature algorithms kagent's admission verifies.
var grantAlgorithms = []jose.SignatureAlgorithm{jose.ES256, jose.ES384, jose.RS256, jose.PS256}

// grantClaims is the grant's payload beside the registered claims, named as
// kagent's admission reads it.
type grantClaims struct {
	Workspace struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"workspace"`
	Volume struct {
		Driver string `json:"driver"`
		Handle string `json:"handle"`
	} `json:"volume"`
	Mounts []struct {
		SubPath  string `json:"sub_path"`
		ReadOnly bool   `json:"read_only"`
	} `json:"mounts"`
}

// Workspace is what a grant admits the caller's Session to.
type Workspace struct {
	// Name is the workspace, <namespace>/<name>.
	Name string
	// Volume is the Session's volume source: the grant's volume, its
	// directories at kagent's fixed layout.
	Volume *apiv1alpha1.SessionVolumeSource
}

// workspaceGrant decodes raw without verifying it, refuses a grant kagent
// would refuse for a reason known here, and answers the workspace it admits
// the caller to. Every refusal names its reason and never the grant.
func workspaceGrant(ctx context.Context, raw string, now time.Time) (*Workspace, error) {
	if len(raw) > MaxWorkspaceGrantLength {
		return nil, invalidf("workspaceGrant is longer than %d characters", MaxWorkspaceGrantLength)
	}
	caller, _ := identity.FromContext(ctx)
	if caller == nil || caller.Email == "" {
		return nil, forbiddenf("a workspace grant names the person by email and the caller has none")
	}
	token, err := jwt.ParseSigned(raw, grantAlgorithms)
	if err != nil {
		return nil, invalidf("workspaceGrant is not a signed workspace grant: %v", err)
	}
	var registered jwt.Claims
	var claims grantClaims
	if err := token.UnsafeClaimsWithoutVerification(&registered, &claims); err != nil {
		return nil, invalidf("workspaceGrant cannot be decoded: %v", err)
	}
	switch {
	case registered.Subject == "":
		return nil, invalidf("the workspace grant names no person (sub)")
	case registered.Subject != caller.Email:
		return nil, forbiddenf("the workspace grant names another person")
	case registered.Expiry == nil:
		return nil, invalidf("the workspace grant has no expiry (exp)")
	case !now.Before(registered.Expiry.Time()):
		return nil, forbiddenf("the workspace grant expired at %s: call authorize_workspace again", registered.Expiry.Time().UTC().Format(time.RFC3339))
	case claims.Volume.Driver == "" || claims.Volume.Handle == "":
		return nil, invalidf("the workspace grant names no volume")
	}
	source := &apiv1alpha1.SessionVolumeSource{
		Volume: &apiv1alpha1.SessionVolume{CsiDriver: claims.Volume.Driver, VolumeHandle: claims.Volume.Handle},
	}
	readWrite, readOnly := 0, 0
	for _, m := range claims.Mounts {
		if m.SubPath == "" {
			return nil, invalidf("the workspace grant allows a mount without a directory (sub_path)")
		}
		mount := &apiv1alpha1.SessionVolumeMount{SubPath: m.SubPath, MountPath: WorkspaceMountPath, ReadOnly: m.ReadOnly}
		if m.ReadOnly {
			mount.MountPath = MirrorsMountPath
			readOnly++
		} else {
			readWrite++
		}
		source.Mounts = append(source.Mounts, mount)
	}
	if readWrite != 1 || readOnly > 1 {
		return nil, invalidf("the workspace grant's mounts are not the workspace layout (the Session's own directory read-write, at most one directory read-only): %d read-write, %d read-only", readWrite, readOnly)
	}
	name := claims.Workspace.Name
	if claims.Workspace.Namespace != "" {
		name = claims.Workspace.Namespace + "/" + name
	}
	return &Workspace{Name: name, Volume: source}, nil
}
