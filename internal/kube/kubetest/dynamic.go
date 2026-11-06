package kubetest

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// NewDynamicClient is an empty fake dynamic client that lists the
// HelmReleases and Agents every read of agent-manager may list (get_info's
// default agent among them), holding objs.
func NewDynamicClient(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Resource: "helmreleases"}: "HelmReleaseList",
		{Group: "api.kagent.dev", Version: "v1alpha3", Resource: "agents"}:         "AgentList",
	}, objs...)
}
