package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/giantswarm/agent-manager/internal/chart"
)

// printer renders the validator's error kinds in English.
var printer = message.NewPrinter(language.English)

// SchemaSource provides the agent chart's values.schema.json.
type SchemaSource interface {
	Schema(ctx context.Context) chart.Schema
}

// ValidateValues checks values against the chart's schema and returns every
// violation as a readable line (path: message). A schema that does not
// compile is reported as one error rather than letting invalid values through.
func ValidateValues(ctx context.Context, src SchemaSource, values map[string]any) (chart.Schema, []string) {
	s := src.Schema(ctx)
	compiled, err := compile(s)
	if err != nil {
		return s, []string{fmt.Sprintf("agent chart schema %s (%s) does not compile: %v", s.Version, s.Source, err)}
	}
	if err := compiled.Validate(values); err != nil {
		return s, flatten(err)
	}
	return s, nil
}

func compile(s chart.Schema) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	// Helm validates values with a JSON-schema library that ignores format
	// assertions; keep parity so the same values pass here and at install.
	const id = "agent-values.schema.json"
	if err := c.AddResource(id, s.Document); err != nil {
		return nil, err
	}
	return c.Compile(id)
}

// flatten turns the validator's error tree into leaf messages, sorted so
// callers (and tests) see a stable list.
func flatten(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	var out []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := "/" + strings.Join(e.InstanceLocation, "/")
			if loc == "/" {
				loc = "(root)"
			}
			out = append(out, fmt.Sprintf("%s: %s", loc, e.ErrorKind.LocalizedString(printer)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	sort.Strings(out)
	return out
}

// valuesCheck is how an agent's values fare: against the chart's values
// schema, then, when they satisfy it, as the objects the chart renders from
// them, dry-run by the API server.
type valuesCheck struct {
	schema chart.Schema
	// violations are the schema's lines, or the chart's or the API server's
	// refusals of the rendered objects.
	violations []string
	// rendered is set once the schema is satisfied: violations are then the
	// chart's or the API server's.
	rendered bool
	// notes say what could not be dry-run, and why.
	notes []string
}

// err is the write's refusal, nil when the values pass.
func (c valuesCheck) err() error {
	switch {
	case len(c.violations) == 0:
		return nil
	case !c.rendered:
		return invalidf("values do not satisfy the agent chart schema %s (%s): %s", c.schema.Version, c.schema.Source, strings.Join(c.violations, "; "))
	default:
		return invalidf("%s", strings.Join(c.violations, "; "))
	}
}

// checkValues judges an agent's values the way its HelmRelease will be
// judged when Flux installs it: the chart's values schema, the chart's own
// refusals while it renders, and a server-side dry-run (dryRun=All) of every
// rendered object as the caller, so the API server's schema, CEL rules and
// admission policies answer — agent-manager copies none of them. An object the
// caller may not write, or a kind the cluster does not serve, is left to Flux
// and named in the notes.
func (s *Service) checkValues(ctx context.Context, st *site, name string, values map[string]any) (valuesCheck, error) {
	sch, violations := ValidateValues(ctx, s.chart, values)
	c := valuesCheck{schema: sch, violations: violations}
	if len(violations) > 0 {
		return c, nil
	}
	c.rendered = true
	objs, err := sch.Render(ctx, name, st.ns(), values)
	if err != nil {
		c.violations = append(c.violations, fmt.Sprintf("agent chart %s (%s): %v", sch.Version, sch.Source, err))
		return c, nil
	}
	for _, obj := range objs {
		violation, note, err := dryRunWrite(ctx, st, obj)
		if err != nil {
			return c, err
		}
		if violation != "" {
			c.violations = append(c.violations, violation)
		}
		if note != "" {
			c.notes = append(c.notes, note)
		}
	}
	return c, nil
}

// requireValidValues is checkValues for a write: the first refusal fails it,
// what could not be dry-run is logged.
func (s *Service) requireValidValues(ctx context.Context, st *site, name string, values map[string]any) error {
	check, err := s.checkValues(ctx, st, name, values)
	if err != nil {
		return err
	}
	for _, note := range check.notes {
		s.log.Warn("agent left to Flux's install for validation", "agent", st.describe(name), "note", note)
	}
	return check.err()
}

// dryRunWrite writes obj with dryRun=All as the caller, the way an install
// or upgrade would: a create when no such object exists, else an update of
// the whole object over the live one (what the render drops is dropped). The
// API server's refusal is the violation. A refusal by RBAC (asked again with
// a SelfSubjectAccessReview, as admission denials are Forbidden too) or a kind
// the cluster does not serve becomes a note.
func dryRunWrite(ctx context.Context, st *site, obj *unstructured.Unstructured) (violation, note string, err error) {
	gvk := obj.GroupVersionKind()
	gvr, _ := meta.UnsafeGuessKindToResource(gvk)
	what := fmt.Sprintf("%s %s/%s", gvk.Kind, obj.GetNamespace(), obj.GetName())
	client := st.agent.Resource(gvr).Namespace(obj.GetNamespace())
	verb := "update"
	live, err := client.Get(ctx, obj.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		verb = "create"
		_, err = client.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager, DryRun: []string{metav1.DryRunAll}})
	case err == nil:
		obj = obj.DeepCopy()
		obj.SetResourceVersion(live.GetResourceVersion())
		_, err = client.Update(ctx, obj, metav1.UpdateOptions{FieldManager: FieldManager, DryRun: []string{metav1.DryRunAll}})
	case apierrors.IsForbidden(err):
		// Without get the live object is unknown; the create answers.
		verb = "create"
		_, err = client.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager, DryRun: []string{metav1.DryRunAll}})
	}
	switch {
	case err == nil:
		return "", "", nil
	case apierrors.IsNotFound(err) && verb == "create":
		return "", fmt.Sprintf("%s was not dry-run: the cluster does not serve %s", what, gvr.GroupResource()), nil
	case apierrors.IsForbidden(err):
		allowed, reviewErr := mayWrite(ctx, st, gvr, obj.GetNamespace(), verb)
		if reviewErr != nil {
			return "", "", wrapKube(reviewErr, "review "+verb+" access to "+gvr.GroupResource().String())
		}
		if !allowed {
			return "", fmt.Sprintf("%s was not dry-run: the caller may not %s %s there; Flux applies it", what, verb, gvr.GroupResource()), nil
		}
		return refusal(what, err), "", nil
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return refusal(what, err), "", nil
	default:
		return "", "", wrapKube(err, "dry-run "+what)
	}
}

// refusal is the API server's answer as one line: its field errors, each
// with its field, else its message. The note that further rules went
// unchecked because of the others is left out.
func refusal(what string, err error) string {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return err.Error()
	}
	var causes []string
	for _, c := range status.Status().Details.Causes {
		if c.Field == "" || c.Field == "<nil>" {
			continue
		}
		causes = append(causes, c.Field+": "+c.Message)
	}
	if len(causes) == 0 {
		return err.Error()
	}
	return fmt.Sprintf("%s is invalid: %s", what, strings.Join(causes, "; "))
}

// mayWrite asks whether the caller may verb gvr in ns.
func mayWrite(ctx context.Context, st *site, gvr schema.GroupVersionResource, ns, verb string) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: ns, Verb: verb, Group: gvr.Group, Resource: gvr.Resource},
	}}
	res, err := st.agentClient.Typed().AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return res.Status.Allowed, nil
}
