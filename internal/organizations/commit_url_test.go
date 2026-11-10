package organizations

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apov1alpha1 "github.com/giantswarm/agent-platform-operator/api/v1alpha1"
	"github.com/giantswarm/gitops-commit/provenance"
)

const hash = "0123456789abcdef0123456789abcdef01234567"

func TestCommitURL(t *testing.T) {
	repository := provenance.Repository{Owner: "giantswarm", Name: "gazelle-configs"}
	for _, tc := range []struct {
		name, revision, want string
	}{
		{"branch", "main@sha1:" + hash, "https://github.com/giantswarm/gazelle-configs/commit/" + hash},
		{"without a branch", "sha1:" + hash, "https://github.com/giantswarm/gazelle-configs/commit/" + hash},
		{"tag holding @", "v1@rc@sha1:" + hash, "https://github.com/giantswarm/gazelle-configs/commit/" + hash},
		{"empty", "", ""},
		{"OCI digest", "latest@sha256:" + hash + hash[:24], ""},
		{"short hash", "main@sha1:" + hash[:12], ""},
		{"not hex", "main@sha1:" + hash[:39] + "/", ""},
		{"upper case", "main@sha1:" + "0123456789ABCDEF0123456789ABCDEF01234567", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, commitURL(repository, tc.revision))
		})
	}
}

func TestSummaryLastAppliedRevision(t *testing.T) {
	org := &apov1alpha1.AgentPlatformOrganization{
		ObjectMeta: metav1.ObjectMeta{Name: "treasury", Namespace: "org-acme"},
		Status:     apov1alpha1.AgentPlatformOrganizationStatus{LastAppliedRevision: "main@sha1:" + hash},
	}

	got := (&Service{}).summary(org)
	assert.Equal(t, "main@sha1:"+hash, got.LastAppliedRevision)
	assert.Empty(t, got.LastAppliedRevisionURL, "without mode commit the repository is unknown")

	commit := &CommitConfig{Objects: provenance.Location{Repository: provenance.Repository{Owner: "giantswarm", Name: "gazelle-configs"}, Branch: "main"}}
	got = (&Service{cfg: Config{Commit: commit}}).summary(org)
	assert.Equal(t, "https://github.com/giantswarm/gazelle-configs/commit/"+hash, got.LastAppliedRevisionURL)
}
