package organizations

import (
	"context"
	"fmt"

	"github.com/giantswarm/gitops-commit/sopsenc"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// CredentialCommit is a credential value to commit into an organization's
// content path. It is never an MCP tool's argument: the Dev Portal's
// credential form posts it to agent-manager's credential route
// (giantswarm/agent-manager#293), which calls CommitCredential.
type CredentialCommit struct {
	// Namespace is the organization namespace.
	Namespace string
	// Secret and Key name where the value goes.
	Secret string
	Key    string
	Value  []byte
	// Replace re-encrypts the Secret's file where git carries it already;
	// without it an existing file is refused. The file holds Key alone: a
	// live Secret with other keys is refused.
	Replace bool
	DryRun  bool
}

// CommitCredential writes a credential Secret into the organization's
// content path as its own secret file (<name>-secret.enc.yaml), encrypted by
// gitops-commit for the recipients of the repository's .sops.yaml, and opens
// the pull request as the caller. A path no creation rule covers is refused,
// never committed in plaintext. The Secret is dry-run live as the caller
// first, so a caller who may not write it is refused before a pull request
// exists. The value is in no answer, log or error.
func (s *Service) CommitCredential(ctx context.Context, req CredentialCommit) (*CommitResult, error) {
	if err := credentialRef(req.Secret, req.Key); err != nil {
		return nil, err
	}
	if len(req.Value) == 0 {
		return nil, invalidf("credential Secret %s key %s: the value is empty", req.Secret, req.Key)
	}
	org, err := s.organization(ctx, req.Namespace)
	if err != nil {
		return nil, err
	}
	w, err := s.contentWriter(ctx, org, agents.ModeCommit)
	if err != nil {
		return nil, err
	}
	if err := segment("Secret", req.Secret); err != nil {
		return nil, err
	}
	p := contentFile(w.dir, kindSecret, req.Secret)
	if !sopsenc.IsSecretFile(p) {
		// The naming convention makes it one; a Secret goes nowhere else.
		return nil, fmt.Errorf("%w: %s is not a secret file by gitops-commit's convention; a Secret is never committed there", agents.ErrInvalid, p)
	}
	c, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	obj := newObject("v1", "Secret", req.Namespace, req.Secret)
	delete(obj.Object, "spec")
	obj.Object["type"] = "Opaque"
	obj.Object["stringData"] = map[string]any{req.Key: string(req.Value)}
	what := fmt.Sprintf("Secret %s/%s", req.Namespace, req.Secret)
	way := wayOrganizationAdmin(req.Namespace)
	wr := write{gvr: secretGVR, kind: kindSecret, op: opCreate, obj: obj.Object, ns: req.Namespace, name: req.Secret, what: what, reason: way}
	if err := apply(ctx, c.Dynamic(), []write{wr}, true); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		// It exists live: the dry run is the update the merge brings.
		live, err := c.Dynamic().Resource(secretGVR).Namespace(req.Namespace).Get(ctx, req.Secret, metav1.GetOptions{})
		if err != nil {
			return nil, kubeErr(err, "get "+what, way)
		}
		// The file holds this one key: other keys of the live Secret
		// would be gone after the merge.
		data, _, _ := unstructured.NestedMap(live.Object, "data")
		for k := range data {
			if k != req.Key {
				return nil, fmt.Errorf("%w: Secret %s holds other keys than %s (%s among them), which a commit of this key alone would remove: give each credential a Secret of its own", agents.ErrConflict, what, req.Key, k)
			}
		}
		obj.SetResourceVersion(live.GetResourceVersion())
		wr.op = opUpdate
		if err := apply(ctx, c.Dynamic(), []write{wr}, true); err != nil {
			return nil, err
		}
	}
	exists, err := w.commit.exists(ctx, p)
	if err != nil {
		return nil, err
	}
	ch := fileChange{dir: w.dir, write: map[string][]byte{p: manifest(obj, false)}}
	switch {
	case exists && !req.Replace:
		return nil, fmt.Errorf("%w: %s carries the credential Secret %s already: replacing it re-encrypts the whole file with the new value (replace)", agents.ErrConflict, p, req.Secret)
	case exists:
		ch.replace = []string{p}
	}
	title := fmt.Sprintf("chore(organizations): set credential %s", req.Secret)
	body := fmt.Sprintf("Sets key %s of the credential Secret %s in organization %s/%s, encrypted for the repository's .sops.yaml recipients.", req.Key, req.Secret, org.Parent, org.Organization)
	res, err := w.commit.open(ctx, []fileChange{ch}, branch("credential-"+req.Secret, org.Parent, org.Organization), title, body, req.DryRun)
	if err != nil {
		return nil, err
	}
	if !req.DryRun {
		s.log.Info("organization credential committed", identity.LogAttr(ctx), "namespace", req.Namespace, "secret", req.Secret, "pullRequest", res.PullRequest)
	}
	return res, nil
}
