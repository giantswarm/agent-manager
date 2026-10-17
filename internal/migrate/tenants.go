package migrate

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/agents"
)

// tenantSkillsGitAuth drops the installation's skills credential, under its
// current or a previous name, from the agent releases in the tenant
// namespaces: that Secret is the installation's, and the tenant credential
// admission policy refuses a reference to it there, so Flux could not apply
// the release's next change. An agent there reads its git sources with its
// own credential or anonymously, as agent-manager composes it. The pass reads
// and writes HelmReleases only (the run's identity holds no more in a
// tenant); a GitOps-owned release is reported, never written.
func (r *Runner) tenantSkillsGitAuth(ctx context.Context) ([]ReleaseReport, bool, []error) {
	installation := agents.ComposeConfig{SkillsGitAuthSecretName: r.opts.SkillsGitAuthSecretName, SkillsGitAuthPreviousSecretNames: r.opts.SkillsGitAuthPreviousSecretNames}
	var (
		reports []ReleaseReport
		changed bool
		errs    []error
	)
	for _, ns := range r.opts.TenantNamespaces {
		res := r.client.Dynamic().Resource(r.helmReleaseGVR()).Namespace(ns)
		list, err := res.List(ctx, metav1.ListOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("list helmreleases in tenant namespace %s: %w", ns, err))
			continue
		}
		for i := range list.Items {
			hr := &list.Items[i]
			name := installationSkillsGitAuth(hr, installation)
			if name == "" {
				continue
			}
			rep := ReleaseReport{Name: hr.GetName(), Namespace: ns, Ownership: OwnershipHelmRelease, ChartVersion: deployedChartVersion(hr),
				Reason: "named the installation's skills credential " + name, Changes: &ValueChanges{Removed: []string{agents.SkillsGitAuthValuesKey}}}
			switch {
			case gitOpsOwned(hr):
				rep.Ownership, rep.Action = OwnershipGitOps, ActionPending
				rep.Reason = fmt.Sprintf("GitOps-owned: remove %s (the installation's %s) in its repository", agents.SkillsGitAuthValuesKey, name)
			case r.opts.DryRun:
				rep.Action, rep.Reason = ActionRewritten, "dry run: would be written; "+rep.Reason
			default:
				err := updateOnConflict(ctx, res, hr.GetName(), func(hr *unstructured.Unstructured) error {
					if installationSkillsGitAuth(hr, installation) == "" {
						return errors.New("skillsGitAuthSecretRef changed since it was read; the next run looks again")
					}
					unstructured.RemoveNestedField(hr.Object, "spec", "values", agents.SkillsGitAuthValuesKey)
					return nil
				})
				if err != nil {
					rep.Action, rep.Reason = ActionFailed, fmt.Sprintf("update refused: %v", err)
					errs = append(errs, fmt.Errorf("update HelmRelease %s/%s: %w", ns, hr.GetName(), err))
					break
				}
				rep.Action, changed = ActionRewritten, true
				r.log.Info("tenant release no longer names the installation's skills credential", "release", ns+"/"+hr.GetName(), "secret", name)
			}
			reports = append(reports, rep)
		}
	}
	return reports, changed, errs
}

// installationSkillsGitAuth is the installation's skills credential name a
// Generic-chart release's skillsGitAuthSecretRef names; empty for any other.
func installationSkillsGitAuth(hr *unstructured.Unstructured, installation agents.ComposeConfig) string {
	if ref := chartRefOf(hr); ref == nil || ref.Kind != "OCIRepository" {
		return ""
	}
	name, _, _ := unstructured.NestedString(hr.Object, "spec", "values", agents.SkillsGitAuthValuesKey, "name")
	if !installation.IsInstallationSkillsGitAuth(name) {
		return ""
	}
	return name
}
