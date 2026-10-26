package config

import (
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	ujconfig "github.com/crossplane/upjet/v2/pkg/config"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane-contrib/provider-openstack/internal/correlationlog"
)

// addCorrelationInitializer prepends the correlation log enrichment initializer
// to every managed resource in pc. It is called from GetProvider and
// GetProviderNamespaced so that the code generator also sees the non-empty
// InitializerFns and emits the loop in every zz_controller.go.
func addCorrelationInitializer(pc *ujconfig.Provider) {
	for name, r := range pc.Resources {
		// Kind and ShortGroup are read when the controller is set up, after the
		// configure loop has run.
		fn := ujconfig.NewInitializerFn(func(_ client.Client) managed.Initializer {
			return correlationlog.NewInitializer(resourceGroupKind(pc.RootGroup, r))
		})
		r.InitializerFns = append([]ujconfig.NewInitializerFn{fn}, r.InitializerFns...)
		pc.Resources[name] = r
	}
}

// resourceGroupKind mirrors how upjet's pipeline derives a resource's API group.
func resourceGroupKind(rootGroup string, r *ujconfig.Resource) schema.GroupKind {
	group := rootGroup
	if r.ShortGroup != "" {
		group = strings.ToLower(r.ShortGroup) + "." + rootGroup
	}
	return schema.GroupKind{Group: group, Kind: r.Kind}
}
