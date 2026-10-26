package config

import (
	"context"
	"testing"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/fake"
	ujconfig "github.com/crossplane/upjet/v2/pkg/config"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane-contrib/provider-openstack/internal/correlationlog"
)

type lineRecorder struct {
	bound []any
	lines *[][]any
}

func (r lineRecorder) Info(_ string, kvs ...any) {
	*r.lines = append(*r.lines, append(append([]any{}, r.bound...), kvs...))
}
func (r lineRecorder) Debug(msg string, kvs ...any) { r.Info(msg, kvs...) }
func (r lineRecorder) WithValues(kvs ...any) logging.Logger {
	return lineRecorder{bound: append(append([]any{}, r.bound...), kvs...), lines: r.lines}
}

// The configure loop sets Kind and ShortGroup after addCorrelationInitializer
// runs, so the initializer must read them when the controller is set up.
func TestCorrelationInitializerUsesTheConfiguredKindWhenTypeMetaIsEmpty(t *testing.T) {
	pc := &ujconfig.Provider{
		RootGroup: "openstack.m.crossplane.io",
		Resources: map[string]*ujconfig.Resource{"openstack_networking_port_v2": {}},
	}
	addCorrelationInitializer(pc)
	r := pc.Resources["openstack_networking_port_v2"]
	r.ShortGroup, r.Kind = "networking", "PortV2"

	mg := &fake.Managed{} // reports an empty kind
	mg.SetNamespace("org-a")
	mg.SetName("port-a")
	mg.SetAnnotations(map[string]string{correlationlog.CorrelationAnnotationKey: `{"owner_org_id":"org-a"}`})
	if err := r.InitializerFns[0](nil).Initialize(context.Background(), mg); err != nil {
		t.Fatal(err)
	}

	lines := &[][]any{}
	gvk := schema.GroupVersionKind{Group: "networking.openstack.m.crossplane.io", Version: "v1alpha1", Kind: "PortV2"}
	log := correlationlog.NewCorrelatingLogger(lineRecorder{lines: lines}).
		WithValues("controller", managed.ControllerName(gvk.String())).
		WithValues("request", reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "org-a", Name: "port-a"}})
	log.Debug("observed")

	line := (*lines)[0]
	for i := 0; i+1 < len(line); i += 2 {
		if line[i] == "owner_org_id" && line[i+1] == "org-a" {
			return
		}
	}
	t.Fatalf("owner_org_id=org-a missing from %v", line)
}
