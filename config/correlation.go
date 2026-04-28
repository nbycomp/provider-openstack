package config

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	ujconfig "github.com/crossplane/upjet/v2/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane-contrib/provider-openstack/internal/correlationlog"
)

// addCorrelationInitializer prepends the correlation log enrichment initializer
// to every managed resource in pc. It is called from GetProvider and
// GetProviderNamespaced so that the code generator also sees the non-empty
// InitializerFns and emits the loop in every zz_controller.go.
func addCorrelationInitializer(pc *ujconfig.Provider) {
	fn := ujconfig.NewInitializerFn(func(_ client.Client) managed.Initializer {
		return correlationlog.NewInitializer()
	})
	for name, r := range pc.Resources {
		r.InitializerFns = append([]ujconfig.NewInitializerFn{fn}, r.InitializerFns...)
		pc.Resources[name] = r
	}
}
