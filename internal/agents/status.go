package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/agent-manager/internal/kube"
)

// errorsIs is errors.Is, named so the service file reads without the import.
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// Container waiting reasons that mean the workload will not recover on its own.
var failedWaitingReasons = map[string]bool{
	"CrashLoopBackOff":                true,
	"ImagePullBackOff":                true,
	"ErrImagePull":                    true,
	"InvalidImageName":                true,
	"CreateContainerConfigError":      true,
	"CreateContainerError":            true,
	"RunContainerError":               true,
	"ErrImageNeverPull":               true,
	"ContainerCannotRun":              true,
	"Error":                           true,
	"OOMKilled":                       true,
	"DeadlineExceeded":                true,
	"StartError":                      true,
	"CreatePodSandboxError":           true,
	"FailedPostStartHook":             true,
	"FailedPreStopHook":               true,
	"KillContainerError":              true,
	"ConfigError":                     true,
	"InvalidEnvironmentVariableNames": true,
}

// Status gathers the Agent CR, the owning HelmRelease (conditions, history),
// the Deployment kagent runs and its pods (waiting reasons — a crash-looping
// pod is phase Running, so containerStatuses are what tell) and the recent
// Warning events, and folds them into one verdict.
func (s *Service) Status(ctx context.Context, ns, name string) (*Status, error) {
	ns, err := s.Namespace(ns)
	if err != nil {
		return nil, err
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	dyn, client, err := s.dyn(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Name: name, Namespace: ns}

	cr, err := s.getAgentCR(ctx, dyn, ns, name)
	if err != nil {
		return nil, err
	}
	hrName, hrNs := name, ns
	if cr != nil {
		conds := conditionsOf(cr)
		st.Agent = &AgentStatus{Exists: true, Conditions: conds, Ready: conditionStatus(conds, "Ready"), Accepted: conditionStatus(conds, "Accepted")}
		if n, nsFromLabel := ownerOf(cr); n != "" {
			hrName, hrNs = n, orDefault(nsFromLabel, ns)
		}
	} else {
		st.Agent = &AgentStatus{Exists: false}
	}

	hr, err := s.getHelmRelease(ctx, dyn, hrNs, hrName)
	if err != nil {
		return nil, err
	}
	if hr != nil {
		st.HelmRelease = helmReleaseStatus(hr)
	} else {
		st.HelmRelease = &HelmReleaseStatus{Exists: false}
	}
	if cr == nil && hr == nil {
		return nil, notFoundf("agent %s/%s: no Agent and no HelmRelease of that name", ns, name)
	}

	// The workload: kagent names the Deployment after the Agent and labels its
	// pods kagent=<name>.
	deploy, err := client.Typed().AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		st.Deployment = &DeploymentStatus{Exists: false}
	case err != nil:
		return nil, wrapKube(err, fmt.Sprintf("get deployment %s/%s", ns, name))
	default:
		ds := &DeploymentStatus{Exists: true, Replicas: deploy.Status.Replicas, ReadyReplicas: deploy.Status.ReadyReplicas, AvailableReplicas: deploy.Status.AvailableReplicas}
		for _, c := range deploy.Status.Conditions {
			ds.Conditions = append(ds.Conditions, Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message, LastTransitionTime: c.LastTransitionTime.UTC().Format("2006-01-02T15:04:05Z")})
		}
		st.Deployment = ds
	}

	pods, err := client.Typed().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "kagent=" + name})
	if err != nil {
		return nil, wrapKube(err, fmt.Sprintf("list pods of %s/%s", ns, name))
	}
	for i := range pods.Items {
		st.Pods = append(st.Pods, podStatus(&pods.Items[i]))
	}
	sort.Slice(st.Pods, func(i, j int) bool { return st.Pods[i].Name < st.Pods[j].Name })

	st.Events = s.warningEvents(ctx, client, ns, name, st.Pods)

	st.Verdict, st.Summary = verdict(st)
	return st, nil
}

func helmReleaseStatus(hr *unstructured.Unstructured) *HelmReleaseStatus {
	conds := conditionsOf(hr)
	out := &HelmReleaseStatus{Exists: true, Conditions: conds, Ready: conditionStatus(conds, "Ready"), GitOpsOwned: gitOpsOwnedHR(hr), Deleting: hr.GetDeletionTimestamp() != nil}
	out.Suspended, _, _ = unstructured.NestedBool(hr.Object, "spec", "suspend")
	out.LastAttemptedRevision, _, _ = unstructured.NestedString(hr.Object, "status", "lastAttemptedRevision")
	if history, found, _ := unstructured.NestedSlice(hr.Object, "status", "history"); found {
		for i, item := range history {
			if i >= 5 {
				break
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			h := HelmReleaseHistory{}
			switch v := m["version"].(type) {
			case int64:
				h.Version = v
			case float64:
				h.Version = int64(v)
			}
			h.ChartVersion, _ = m["chartVersion"].(string)
			h.Status, _ = m["status"].(string)
			h.LastDeployed, _ = m["lastDeployed"].(string)
			out.History = append(out.History, h)
		}
	}
	return out
}

func podStatus(pod *corev1.Pod) PodStatus {
	ps := PodStatus{Name: pod.Name, Phase: string(pod.Status.Phase)}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ps.Ready = true
		}
	}
	collect := func(statuses []corev1.ContainerStatus, init bool) {
		for _, cs := range statuses {
			ps.Restarts += cs.RestartCount
			switch {
			case cs.State.Waiting != nil:
				ps.Containers = append(ps.Containers, ContainerStatus{Name: cs.Name, State: "waiting", Reason: cs.State.Waiting.Reason, Message: cs.State.Waiting.Message, Init: init})
			case cs.State.Terminated != nil && (!init || cs.State.Terminated.ExitCode != 0):
				ps.Containers = append(ps.Containers, ContainerStatus{Name: cs.Name, State: "terminated", Reason: cs.State.Terminated.Reason, Message: cs.State.Terminated.Message, Init: init})
			case cs.State.Running != nil && !cs.Ready && !init:
				ps.Containers = append(ps.Containers, ContainerStatus{Name: cs.Name, State: "running", Reason: "NotReady", Init: init})
			}
		}
	}
	collect(pod.Status.InitContainerStatuses, true)
	collect(pod.Status.ContainerStatuses, false)
	return ps
}

// warningEvents lists recent Warning events on the agent's objects (Agent,
// HelmRelease, Deployment, ReplicaSets and pods named after it), newest
// first, at most ten. Events are diagnostics: a list failure yields none.
func (s *Service) warningEvents(ctx context.Context, client kube.Client, ns, name string, pods []PodStatus) []Event {
	list, err := client.Typed().CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=Warning"})
	if err != nil {
		s.log.Debug("listing events failed", "namespace", ns, "error", err)
		return nil
	}
	podNames := map[string]bool{}
	for _, p := range pods {
		podNames[p.Name] = true
	}
	var out []Event
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.Type != corev1.EventTypeWarning {
			continue
		}
		obj := ev.InvolvedObject
		owned := obj.Name == name || podNames[obj.Name] ||
			((obj.Kind == "ReplicaSet" || obj.Kind == "Pod") && strings.HasPrefix(obj.Name, name+"-"))
		if !owned {
			continue
		}
		last := ev.LastTimestamp.Time
		if last.IsZero() {
			last = ev.EventTime.Time
		}
		if last.IsZero() {
			last = ev.CreationTimestamp.Time
		}
		e := Event{Type: ev.Type, Reason: ev.Reason, Message: ev.Message, Object: obj.Kind + "/" + obj.Name, Count: ev.Count}
		if !last.IsZero() {
			e.Last = last.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// verdict folds the gathered facts into ready / progressing / failed / unknown
// and one actionable sentence.
func verdict(st *Status) (string, string) {
	if st.HelmRelease != nil && st.HelmRelease.Deleting {
		return VerdictProgressing, "HelmRelease is being uninstalled by helm-controller; the Agent disappears with it"
	}
	if st.HelmRelease != nil && st.HelmRelease.Exists && st.HelmRelease.Ready != nil && !*st.HelmRelease.Ready {
		c := findCondition(st.HelmRelease.Conditions, "Ready")
		reason, msg := "", ""
		if c != nil {
			reason, msg = c.Reason, c.Message
		}
		return VerdictFailed, fmt.Sprintf("HelmRelease is not ready (%s): %s", reason, msg)
	}
	for _, pod := range st.Pods {
		for _, c := range pod.Containers {
			if failedWaitingReasons[c.Reason] {
				return VerdictFailed, fmt.Sprintf("pod %s container %s is %s: %s", pod.Name, c.Name, c.Reason, firstLine(c.Message))
			}
		}
	}
	if st.Agent != nil && st.Agent.Exists && st.Agent.Accepted != nil && !*st.Agent.Accepted {
		c := findCondition(st.Agent.Conditions, "Accepted")
		return VerdictFailed, "kagent rejected the Agent configuration: " + conditionText(c)
	}
	if st.Agent != nil && st.Agent.Exists && st.Agent.Ready != nil && *st.Agent.Ready {
		if st.Deployment == nil || !st.Deployment.Exists || st.Deployment.AvailableReplicas > 0 || st.Deployment.Replicas == 0 {
			return VerdictReady, fmt.Sprintf("Agent is Ready; %d/%d pods available", availableOf(st), replicasOf(st))
		}
	}
	for _, ev := range st.Events {
		if strings.Contains(ev.Reason, "Failed") || ev.Reason == "BackOff" {
			return VerdictProgressing, fmt.Sprintf("not ready yet; last warning on %s: %s: %s", ev.Object, ev.Reason, firstLine(ev.Message))
		}
	}
	if st.HelmRelease != nil && st.HelmRelease.Exists && st.HelmRelease.Ready == nil {
		return VerdictProgressing, "HelmRelease created; Flux has not reconciled it yet"
	}
	if st.Agent != nil && !st.Agent.Exists && st.HelmRelease != nil && st.HelmRelease.Exists {
		return VerdictProgressing, "HelmRelease is ready but the Agent has not been rendered yet"
	}
	if st.Agent != nil && st.Agent.Exists && st.Agent.Ready == nil {
		return VerdictProgressing, "Agent accepted; kagent has not reported readiness yet"
	}
	if st.Deployment != nil && st.Deployment.Exists && st.Deployment.AvailableReplicas == 0 {
		return VerdictProgressing, fmt.Sprintf("Deployment has %d/%d pods available", st.Deployment.AvailableReplicas, st.Deployment.Replicas)
	}
	if st.Agent != nil && st.Agent.Exists {
		c := findCondition(st.Agent.Conditions, "Ready")
		return VerdictProgressing, "Agent is not ready: " + conditionText(c)
	}
	return VerdictUnknown, "no Agent and no HelmRelease reported anything yet"
}

func availableOf(st *Status) int32 {
	if st.Deployment == nil {
		return 0
	}
	return st.Deployment.AvailableReplicas
}

func replicasOf(st *Status) int32 {
	if st.Deployment == nil {
		return 0
	}
	return st.Deployment.Replicas
}

func conditionText(c *Condition) string {
	if c == nil {
		return "no condition reported"
	}
	if c.Message != "" {
		return c.Reason + ": " + c.Message
	}
	return c.Reason
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
