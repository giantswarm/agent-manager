package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/giantswarm/agent-manager/internal/identity"
	apiv1alpha1 "github.com/giantswarm/agent-manager/internal/kagent/gen/kagent/api/v1alpha1"
	"github.com/giantswarm/agent-manager/internal/kagent/kagenttest"
)

// asPerson is the caller as the OAuth layer attaches them: the identity the
// grant must name and the IdP token the session operations present.
func asPerson(ctx context.Context, email string) context.Context {
	return identity.ContextWithToken(identity.ContextWith(ctx, &identity.Identity{Subject: "dex-" + email, Email: email}), callerToken)
}

// platformVolume is the volume source the workspace grant of
// kagenttest.WorkspaceGrant composes: its volume, the session's own directory
// read-write at /workspace and the mirrors read-only at /mirrors.
func platformVolume() *apiv1alpha1.SessionVolumeSource {
	return &apiv1alpha1.SessionVolumeSource{
		Volume: &apiv1alpha1.SessionVolume{CsiDriver: "nfs.csi.k8s.io", VolumeHandle: "nfs-server.agentlab.svc.cluster.local#export#workspace-platform#"},
		Mounts: []*apiv1alpha1.SessionVolumeMount{
			{SubPath: "sessions/${SESSION_ID}", MountPath: "/workspace"},
			{SubPath: "mirrors", MountPath: "/mirrors", ReadOnly: true},
		},
	}
}

func createRequests(ctrl *kagenttest.Controller) []*apiv1alpha1.CreateSessionRequest {
	var out []*apiv1alpha1.CreateSessionRequest
	for _, call := range ctrl.Calls() {
		if req, ok := call.Request.(*apiv1alpha1.CreateSessionRequest); ok {
			out = append(out, req)
		}
	}
	return out
}

func TestStartSessionWithoutAGrantIsTodaysRequest(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := asPerson(t.Context(), "admin@lab.local")

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1", SessionName: "triage"})
	require.NoError(t, err)
	assert.Empty(t, started.Workspace)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	want := &apiv1alpha1.CreateSessionRequest{
		Agent:     &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "sre"},
		RequestId: "req-1",
		Name:      "triage",
	}
	assert.True(t, proto.Equal(want, reqs[0]), "the request is the one the surfaces send today: %v", reqs[0])
	assert.Nil(t, reqs[0].GetVolumeSource())
	assert.Empty(t, reqs[0].GetAdmissionToken())
}

func TestStartSessionWithAGrantCarriesTheAdmissionTokenAndVolumeSource(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())

	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", RequestID: "req-1", SessionName: "triage", WorkspaceGrant: grant})
	require.NoError(t, err)
	assert.Equal(t, "kagent/platform", started.Workspace)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 1)
	want := &apiv1alpha1.CreateSessionRequest{
		Agent:          &apiv1alpha1.ResourceReference{Namespace: "kagent", Name: "sre"},
		RequestId:      "req-1",
		Name:           "triage",
		VolumeSource:   platformVolume(),
		AdmissionToken: grant,
	}
	assert.True(t, proto.Equal(want, reqs[0]), "the request carries the grant as the admission token and the volume source it names: %v", reqs[0])

	var session struct {
		VolumeSource map[string]any `json:"volumeSource"`
	}
	require.NoError(t, json.Unmarshal(started.Session, &session))
	assert.Equal(t, "nfs.csi.k8s.io", session.VolumeSource["volume"].(map[string]any)["csiDriver"], "the session shows its volume source")
	assert.NotContains(t, string(started.Session), grant)
	assert.NotContains(t, string(started.Turn), grant)
}

func TestStartSessionRefusesAGrantKagentWouldRefuse(t *testing.T) {
	signer := kagenttest.NewSigner(t)
	noVolume := kagenttest.WorkspaceGrant()
	noVolume.Volume.Handle = ""
	twoDirectories := kagenttest.WorkspaceGrant()
	twoDirectories.Mounts = append(twoDirectories.Mounts, kagenttest.GrantMount{SubPath: "scratch"})
	for name, test := range map[string]struct {
		ctx   context.Context
		grant string
		want  error
		says  string
	}{
		"another person's": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: signer.Sign(t, "dev@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant()),
			want: ErrForbidden, says: "names another person",
		},
		"expired": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: signer.Sign(t, "admin@lab.local", -time.Minute, kagenttest.WorkspaceGrant()),
			want: ErrForbidden, says: "expired at",
		},
		"not a JWT": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: "not-a-grant",
			want: ErrInvalid, says: "not a signed workspace grant",
		},
		"a tampered payload": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: tamper(signer.Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())),
			want: ErrInvalid, says: "not a signed workspace grant",
		},
		"claims of the wrong shape": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: rawJWT(`{"sub":"admin@lab.local","exp":"soon"}`),
			want: ErrInvalid, says: "cannot be decoded",
		},
		"no subject": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: rawJWT(`{"exp":4102444800}`),
			want: ErrInvalid, says: "names no person",
		},
		"no expiry": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: rawJWT(`{"sub":"admin@lab.local"}`),
			want: ErrInvalid, says: "has no expiry",
		},
		"no volume": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: signer.Sign(t, "admin@lab.local", 5*time.Minute, noVolume),
			want: ErrInvalid, says: "names no volume",
		},
		"not the workspace layout": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: signer.Sign(t, "admin@lab.local", 5*time.Minute, twoDirectories),
			want: ErrInvalid, says: "not the workspace layout",
		},
		"too long": {
			ctx: asPerson(t.Context(), "admin@lab.local"), grant: strings.Repeat("x", MaxWorkspaceGrantLength+1),
			want: ErrInvalid, says: "longer than",
		},
		"a caller without an email": {
			ctx: identity.ContextWithToken(t.Context(), callerToken), grant: signer.Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant()),
			want: ErrForbidden, says: "the caller has none",
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, ctrl := newSessionFixture(t)
			_, err := svc.StartSession(test.ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: test.grant})
			require.ErrorIs(t, err, test.want)
			assert.Contains(t, err.Error(), test.says)
			assert.NotContains(t, err.Error(), test.grant[:min(16, len(test.grant))], "the refusal never carries the grant")
			assert.Empty(t, ctrl.Calls(), "a refused grant creates no session")
		})
	}
}

// tamper flips the payload of a compact JWT so it no longer decodes.
func tamper(token string) string {
	parts := strings.Split(token, ".")
	parts[1] = "!" + parts[1][1:]
	return strings.Join(parts, ".")
}

// rawJWT is a compact JWT with the payload as given and a signature nobody
// verifies here: what a decoder reads, whatever signed it.
func rawJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"ES256","typ":"JWT","kid":"test"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("signature"))
}

func TestStartSessionReturnsTheAdmissionsRefusal(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	ctrl.Admission = func(*apiv1alpha1.CreateSessionRequest) error {
		return status.Error(codes.PermissionDenied, "admission grant names another volume")
	}

	_, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.ErrorIs(t, err, ErrForbidden)
	assert.Contains(t, err.Error(), "admission grant names another volume")
	assert.NotContains(t, err.Error(), grant)
	list, err := svc.ListSessions(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.Empty(t, list.Sessions, "a refused admission created no session")
}

func TestStartSessionWithAGrantTwiceIsOneSession(t *testing.T) {
	svc, ctrl := newSessionFixture(t)
	ctx := asPerson(t.Context(), "admin@lab.local")
	grant := kagenttest.NewSigner(t).Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	in := StartSession{Name: "sre", Message: "hello", RequestID: "req-1", WorkspaceGrant: grant}

	first, err := svc.StartSession(ctx, in)
	require.NoError(t, err)
	again, err := svc.StartSession(ctx, in)
	require.NoError(t, err)
	assert.JSONEq(t, string(first.Session), string(again.Session), "the same request id answers the same session")
	list, err := svc.ListSessions(ctx, "", "sre", false)
	require.NoError(t, err)
	assert.Len(t, list.Sessions, 1)
	reqs := createRequests(ctrl)
	require.Len(t, reqs, 2)
	assert.True(t, proto.Equal(reqs[0], reqs[1]), "the retry sent the request of the first attempt")
}

func TestTheGrantReachesNoLogLineOrError(t *testing.T) {
	var logged bytes.Buffer
	svc, ctrl := newSessionFixture(t)
	svc.log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := asPerson(t.Context(), "admin@lab.local")
	signer := kagenttest.NewSigner(t)
	grant := signer.Sign(t, "admin@lab.local", 5*time.Minute, kagenttest.WorkspaceGrant())
	expired := signer.Sign(t, "admin@lab.local", -time.Minute, kagenttest.WorkspaceGrant())
	var errs []string

	// The paths a grant takes: admitted, refused here, refused by kagent's
	// admission, and a first message the runtime did not take.
	started, err := svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.NoError(t, err)
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: expired})
	require.Error(t, err)
	errs = append(errs, err.Error())
	ctrl.Admission = func(*apiv1alpha1.CreateSessionRequest) error {
		return status.Error(codes.PermissionDenied, "admission grant has expired")
	}
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.Error(t, err)
	errs = append(errs, err.Error())
	ctrl.Admission = nil
	ctrl.Fail["SendStreamingMessage"] = status.Error(codes.Unavailable, "runtime unavailable")
	_, err = svc.StartSession(ctx, StartSession{Name: "sre", Message: "hello", WorkspaceGrant: grant})
	require.Error(t, err)
	errs = append(errs, err.Error())

	for _, token := range []string{grant, expired} {
		for _, part := range strings.Split(token, ".") {
			assert.NotContains(t, logged.String(), part, "no log line carries the grant")
			assert.NotContains(t, string(started.Session), part)
			for _, e := range errs {
				assert.NotContains(t, e, part, "no error carries the grant")
			}
		}
	}
	assert.Contains(t, logged.String(), `"workspace":"kagent/platform"`, "the start names the workspace")
}

func TestWorkspaceGrantMountsAreTheFixedLayout(t *testing.T) {
	ctx := asPerson(t.Context(), "admin@lab.local")
	signer := kagenttest.NewSigner(t)
	ownDirectoryOnly := kagenttest.WorkspaceGrant()
	ownDirectoryOnly.Mounts = ownDirectoryOnly.Mounts[:1]
	ws, err := workspaceGrant(ctx, signer.Sign(t, "admin@lab.local", time.Minute, ownDirectoryOnly), time.Now())
	require.NoError(t, err)
	assert.Equal(t, "kagent/platform", ws.Name)
	require.Len(t, ws.Volume.GetMounts(), 1)
	assert.Equal(t, WorkspaceMountPath, ws.Volume.GetMounts()[0].GetMountPath())
	assert.False(t, ws.Volume.GetMounts()[0].GetReadOnly())

	mirrorsOnly := kagenttest.WorkspaceGrant()
	mirrorsOnly.Mounts = mirrorsOnly.Mounts[1:]
	_, err = workspaceGrant(ctx, signer.Sign(t, "admin@lab.local", time.Minute, mirrorsOnly), time.Now())
	require.ErrorIs(t, err, ErrInvalid)
	assert.Contains(t, err.Error(), "0 read-write, 1 read-only")
}
