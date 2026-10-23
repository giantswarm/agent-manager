package migrate

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// ConvergeSources moves the agent chart's shared OCIRepository of each
// namespace to the configured range and tag filter, the one a new source is
// created with, when agent-manager starts. Only a source agent-manager created
// in the cluster is written, and only within its major: a move to another
// major rewrites the releases' values first, which is this package's migrate
// command. A source already on the range is not written. Each namespace's
// outcome is returned and logged; nothing here fails the start.
func ConvergeSources(ctx context.Context, dyn dynamic.Interface, namespaces []string, cfg agents.ComposeConfig, log *slog.Logger) []SourceReport {
	want := agents.BuildOCIRepository("", cfg)
	semver, _, _ := unstructured.NestedString(want.Object, "spec", "ref", "semver")
	filter, _, _ := unstructured.NestedString(want.Object, "spec", "ref", "semverFilter")
	url, _, _ := unstructured.NestedString(want.Object, "spec", "url")
	res := dyn.Resource(gvrFor(want.GetAPIVersion(), "ocirepositories"))

	var out []SourceReport
	for _, ns := range namespaces {
		obj, err := res.Namespace(ns).Get(ctx, want.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		rep := SourceReport{Name: want.GetName(), Namespace: ns, Ownership: OwnershipHelmRelease, To: semver, ToSemverFilter: filter}
		if err != nil {
			rep.Action, rep.Reason = SourceNotMoved, fmt.Sprintf("get refused: %v", err)
			out = append(out, logSource(log, rep))
			continue
		}
		rep.From, _, _ = unstructured.NestedString(obj.Object, "spec", "ref", "semver")
		rep.FromSemverFilter, _, _ = unstructured.NestedString(obj.Object, "spec", "ref", "semverFilter")
		rep.Action, rep.Reason = convergeAction(obj, url, rep)
		if rep.Action == SourceMoved {
			if err := updateOnConflict(ctx, res.Namespace(ns), rep.Name, func(o *unstructured.Unstructured) error {
				setRef(o, semver, filter)
				return nil
			}); err != nil {
				rep.Action, rep.Reason = SourceNotMoved, fmt.Sprintf("update refused: %v", err)
			}
		}
		out = append(out, logSource(log, rep))
	}
	return out
}

// convergeAction decides whether a source moves to rep.To within its major.
func convergeAction(obj *unstructured.Unstructured, url string, rep SourceReport) (string, string) {
	if u, _, _ := unstructured.NestedString(obj.Object, "spec", "url"); u != url {
		return SourceNotMoved, fmt.Sprintf("points at %s, not the agent chart %s", u, url)
	}
	if gitOpsOwned(obj) {
		return SourceNotMoved, "its desired state lives in a GitOps repository: move the range there"
	}
	if obj.GetLabels()[agents.ManagedByLabel] != agents.ManagedByValue {
		return SourceNotMoved, "not created by agent-manager"
	}
	if rep.From == rep.To && rep.FromSemverFilter == rep.ToSemverFilter {
		return ActionUnchanged, "already on the configured range"
	}
	if rep.From == "" {
		return SourceNotMoved, "spec.ref carries no semver range (a tag or digest pin); move it by hand"
	}
	to, err := TargetMajor(rep.To)
	if err != nil {
		return SourceNotMoved, err.Error()
	}
	if from, err := TargetMajor(rep.From); err != nil || from != to {
		return SourceNotMoved, fmt.Sprintf("%s is not on the %d.x line of %s: the migrate command moves it with its releases", rep.From, to, rep.To)
	}
	return SourceMoved, ""
}

func logSource(log *slog.Logger, rep SourceReport) SourceReport {
	attrs := []any{"source", rep.Namespace + "/" + rep.Name, "from", rep.From, "to", rep.To, "fromSemverFilter", rep.FromSemverFilter, "toSemverFilter", rep.ToSemverFilter}
	switch rep.Action {
	case SourceMoved:
		log.Info("chart source moved to the configured range", attrs...)
	case SourceNotMoved:
		log.Warn("chart source not moved to the configured range", append(attrs, "reason", rep.Reason)...)
	}
	return rep
}
