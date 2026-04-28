# Reconciliation Log Enrichment

This document describes the changes required to add per-reconcile structured log
enrichment to this provider. When applied, every meaningful log line emitted
during reconciliation of a managed resource will automatically include any
key-value pairs stored in the resource's `nby.one/correlation_id` annotation.

## Annotation format

```yaml
metadata:
  annotations:
    nby.one/correlation_id: '{"deployment_id": "d-1234", "team": "platform"}'
```

The value must be a flat JSON object. Keys that would clash with standard
controller-runtime log fields (`controller`, `controllerKind`, `name`,
`namespace`, `reconcileID`) are automatically prefixed with `correlation_id_`.

## How it works

Two cooperating components are wired together:

1. **`CorrelatingLogger`** — a `logging.Logger` wrapper set as the provider's
   top-level logger. On every `Info` / `Debug` call it reads a goroutine-local
   `sync.Map` and prepends any correlation fields stored there. Because
   `WithValues` propagates the same underlying logger (and the map is
   package-level), all derived loggers — including the managed reconciler's
   internal `r.log` set via `managed.WithLogger(o.Logger.WithValues(...))` —
   go through this wrapper.

2. **`correlationInitializer`** — a `managed.Initializer` registered as the
   first initializer on every controller. At the start of each reconcile cycle,
   after the managed resource has been fetched from the API server, it parses
   the annotation and writes the fields into the shared goroutine-local map.
   From that point onwards every log call on the same goroutine carries the
   fields.

Controller-runtime worker goroutines each process one reconcile at a time, so
keying by goroutine ID is safe: the entry is overwritten at the start of the
next reconcile for that goroutine, and no cleanup is needed.

The initializer is registered via upjet's `InitializerFns` mechanism, which is
already supported by the code-generation template. Adding a non-empty
`InitializerFns` slice to every resource config causes the generator to emit the
corresponding loop in every `zz_controller.go`.

## Known limitations

| Lines | Enriched? | Reason |
|---|---|---|
| All post-`Initialize` reconcile lines (Connect, Observe, Create, Update, Delete …) | ✅ | `r.log` goes through `CorrelatingLogger`; fields set by the initializer |
| Pre-`Initialize` debug lines (`"Reconciling"` etc.) on the **first** reconcile cycle | ⚠️ | `Initialize` has not yet run; all are `Debug` level, suppressed in production. Subsequent cycles see the fields because the goroutine-local entry persists from the previous cycle. |
| Orphan-deletion path (`DeletionPolicy: Orphan`) | ⚠️ | Skips `Initialize` entirely |
| Upjet async Terraform worker goroutine | ⚠️ | Spawned by `OperationTrackerStore`; goroutine-local fields do not follow across goroutine boundaries |
| Event-handler callbacks (`"Calling the inner handler …"`) | ⚠️ | Fired from informer watch goroutines, not the reconcile goroutine |

## Changes required

### 1. New file — `internal/correlationlog/correlationlog.go`

Create the file with the following content. No new external dependencies are
introduced; all imports are already present in the module.

```go
package correlationlog

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	xpresource "github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

const (
	// CorrelationAnnotationKey is the annotation used to carry correlation
	// metadata as a JSON object for structured log enrichment.
	CorrelationAnnotationKey = "nby.one/correlation_id"

	reservedFieldPrefix = "correlation_id_"
)

var reservedLogFieldKeys = map[string]struct{}{
	"controller":     {},
	"controllerKind": {},
	"name":           {},
	"namespace":      {},
	"reconcileID":    {},
}

// sharedFields is the single goroutine-local store used by all
// CorrelatingLogger instances and all correlationInitializer instances in this
// process. Keyed by goroutine ID (uint64), values are []any kv slices.
var sharedFields sync.Map

// NewCorrelatingLogger wraps base so that every Info and Debug call
// automatically prepends the per-goroutine correlation fields written by the
// correlationInitializer at the start of each reconcile. Wire it around the
// top-level logger in main.go.
func NewCorrelatingLogger(base logging.Logger) *CorrelatingLogger {
	return &CorrelatingLogger{base: base}
}

// NewInitializer returns a managed.Initializer that, on each reconcile,
// parses the correlation annotation of the managed resource and stores the
// fields in the shared goroutine-local store so that every subsequent log call
// on any CorrelatingLogger picks them up.
func NewInitializer() managed.Initializer {
	return &correlationInitializer{}
}

// CorrelatingLogger wraps a logging.Logger and prepends per-goroutine
// correlation fields on every Info and Debug call.
type CorrelatingLogger struct {
	base logging.Logger
}

// Info implements logging.Logger.
func (l *CorrelatingLogger) Info(msg string, kvs ...any) {
	l.base.Info(msg, enrich(kvs)...)
}

// Debug implements logging.Logger.
func (l *CorrelatingLogger) Debug(msg string, kvs ...any) {
	l.base.Debug(msg, enrich(kvs)...)
}

// WithValues implements logging.Logger.
func (l *CorrelatingLogger) WithValues(kvs ...any) logging.Logger {
	return &CorrelatingLogger{base: l.base.WithValues(kvs...)}
}

func enrich(kvs []any) []any {
	v, ok := sharedFields.Load(currentGoroutineID())
	if !ok {
		return kvs
	}
	corr := v.([]any)
	out := make([]any, 0, len(corr)+len(kvs))
	return append(append(out, corr...), kvs...)
}

// correlationInitializer implements managed.Initializer.
type correlationInitializer struct{}

func (correlationInitializer) Initialize(_ context.Context, mg xpresource.Managed) error {
	values, err := extractCorrelationValues(mg.GetAnnotations())
	if err != nil || len(values) == 0 {
		return nil
	}
	keys := sortedKeys(values)
	kvs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		kvs = append(kvs, logFieldKey(k), values[k])
	}
	sharedFields.Store(currentGoroutineID(), kvs)
	return nil
}

// currentGoroutineID returns the ID of the calling goroutine by parsing the
// first line of a runtime stack trace ("goroutine NNN [...]"). This format
// has been stable across all Go versions and is widely used in practice.
func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	fields := strings.Fields(string(buf[:n]))
	if len(fields) < 2 {
		return 0
	}
	id, _ := strconv.ParseUint(fields[1], 10, 64)
	return id
}

func extractCorrelationValues(annotations map[string]string) (map[string]string, error) {
	if len(annotations) == 0 {
		return nil, nil
	}
	raw := strings.TrimSpace(annotations[CorrelationAnnotationKey])
	if raw == "" {
		return nil, nil
	}
	parsed := make(map[string]any)
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", CorrelationAnnotationKey, err)
	}
	values := make(map[string]string, len(parsed))
	for k, v := range parsed {
		if strings.TrimSpace(k) == "" {
			continue
		}
		values[k] = stringify(v)
	}
	if len(values) == 0 {
		return nil, nil
	}
	return values, nil
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

func logFieldKey(key string) string {
	if _, reserved := reservedLogFieldKeys[key]; reserved {
		return reservedFieldPrefix + key
	}
	return key
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

### 2. New file — `config/correlation.go`

```go
package config

import (
	"github.com/crossplane-contrib/provider-openstack/internal/correlationlog"
	ujconfig "github.com/crossplane/upjet/v2/pkg/config"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
```

### 3. Edit `config/provider.go`

Call `addCorrelationInitializer` in both `GetProvider` and `GetProviderNamespaced`,
immediately after the existing `bumpVersionsWithEmbeddedLists` call and before
the per-resource `configure` loop:

```diff
 	bumpVersionsWithEmbeddedLists(pc)
+	addCorrelationInitializer(pc)
 	for _, configure := range []func(provider *ujconfig.Provider){ ... } {
 		configure(pc)
 	}
```

Apply the same diff to both functions. This placement is required so that the
code generator — which calls `GetProvider(ctx, true)` and
`GetProviderNamespaced(ctx, true)` directly — also sees the non-empty
`InitializerFns` and emits the initializer loop in every generated
`zz_controller.go`.

### 4. Edit `cmd/provider/main.go`

Wrap the top-level logger immediately after it is constructed:

```diff
+	"github.com/crossplane-contrib/provider-openstack/internal/correlationlog"
 	...

-	logr := logging.NewLogrLogger(zl.WithName("provider-openstack"))
+	baseLogger := logging.NewLogrLogger(zl.WithName("provider-openstack"))
+	logr := correlationlog.NewCorrelatingLogger(baseLogger)
```

Because both `optionsCluster.Options.Logger` and `optionsNamespaced.Options.Logger`
are assigned from `logr`, this single change covers every controller in both
provider scopes.

### 5. Regenerate controllers

The upjet code-generation template already contains the `InitializerFns` loop,
guarded by `{{- if .Initializers }}`. Steps 3 and 4 above make that condition
true for every resource, so running the generator produces a `zz_controller.go`
for each resource that prepends the correlation initializer before
`NewNameAsExternalName`.

Install goimports (required) with:

```sh
go install golang.org/x/tools/cmd/goimports@latest
```

The generator default for `--generated-resource-list` is relative to the
generator binary's working directory, not the repo root, so the path must be
supplied explicitly:

```sh
go run cmd/generator/main.go \
  --generated-resource-list="$PWD/config/generated.lst" \
  "$PWD"
```

After generation, verify the loop is present in a sample controller:

```sh
grep -A3 "InitializerFns" \
  internal/controller/namespaced/images/imagev2/zz_controller.go
```

Expected output:

```go
for _, i := range o.Provider.Resources["openstack_images_image_v2"].InitializerFns {
    initializers = append(initializers, i(mgr.GetClient()))
}
```
