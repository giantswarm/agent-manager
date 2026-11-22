package organizations

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/agent-manager/internal/kube"
)

func TestOrganizationReviewsFailClosed(t *testing.T) {
	for _, name := range []string{"allowed", "denied", "evaluation error", "contradictory", "expired caller"} {
		t.Run(name, func(t *testing.T) {
			typed := kubefake.NewClientset()
			typed.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if name == "expired caller" {
					return true, nil, apierrors.NewUnauthorized("expired")
				}
				review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				require.Equal(t, "organization", review.Spec.ResourceAttributes.Namespace)
				require.Equal(t, "list", review.Spec.ResourceAttributes.Verb)
				review.Status.Allowed = name != "denied"
				review.Status.Denied = name == "denied" || name == "contradictory"
				if name == "evaluation error" {
					review.Status.EvaluationError = "webhook unavailable"
				}
				return true, review, nil
			})
			client := kube.FromInterfaces(nil, typed, nil)
			allowed, err := reviewAccess(t.Context(), client, access{namespace: "organization", verb: "list", resource: "agents"})
			require.Equal(t, name == "allowed", allowed)
			if name == "evaluation error" || name == "expired caller" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			service := &Service{log: slog.New(slog.DiscardHandler)}
			allowed, known, err := service.allowed(t.Context(), client, authorizationv1.ResourceAttributes{Namespace: "organization", Verb: "list", Resource: "agents"})
			require.Equal(t, name == "allowed", allowed)
			require.Equal(t, name != "evaluation error" && name != "expired caller", known)
			if name == "expired caller" {
				require.ErrorIs(t, err, errTokenRefused)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
