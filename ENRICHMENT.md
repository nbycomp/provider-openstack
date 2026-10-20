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

The value must be a flat JSON object. Keys that would clash with fields the
reconciler, upjet or controller-runtime already log (`controller`,
`controllerKind`, `name`, `namespace`, `reconcileID`, `request`, `uid`, `gvk`,
`trackerUID`, `resourceName`, `resourceNamespace`, `error`, `external-name`,
`version`) are prefixed with `correlation_id_`. A malformed annotation is
ignored and reported once per reconcile with `faultDomain: input`; it never
fails the reconcile.

## How it works

Two cooperating components are wired together:

1. **`correlationInitializer`**: a `managed.Initializer` registered as the
   first initializer on every controller. On each reconcile it parses the
   annotation and stores the fields in a package-level map keyed by the managed
   resource (API group, kind, namespace, name). A resource with no annotation,
   or a malformed one, has its entry removed.

2. **`CorrelatingLogger`**: a `logging.Logger` wrapper set as the provider's
   top-level logger. The managed reconciler and upjet already attach the
   resource's identity through `WithValues`:
   - the reconciler adds `controller` and `request`
   - the external client adds `name`, `namespace` and `gvk`
   - the async operation tracker adds `resourceName`, `resourceNamespace` and `gvk`

   The wrapper binds to that identity and adds the stored fields of that
   resource, and only that resource, to every `Info` and `Debug` line.
   Values passed at call time only fill identity parts that are still unknown,
   so a line that mentions another object's `name` keeps its own resource.

Because the store is keyed by resource rather than by goroutine, a line never
carries another resource's fields. Lines logged outside the reconcile
goroutine, such as the async create, update and delete callbacks, are enriched
too.

The initializer is registered via upjet's `InitializerFns` mechanism, which is
already supported by the code-generation template. Adding a non-empty
`InitializerFns` slice to every resource config causes the generator to emit the
corresponding loop in every `zz_controller.go`.

## Logging output

`--log-format` selects `json` (default) or `console`. `--debug` sets only the
level, so the debug lines that carry most of the reconcile detail can be
shipped as JSON.

## Known limitations

| Lines | Enriched? | Reason |
|---|---|---|
| Reconcile, external client and async callback lines | ✅ | Bound to the resource through the identity keys above |
| Lines logged before the first `Initialize` of a resource | ⚠️ | Nothing is stored yet; they carry no correlation fields, never another resource's |
| Orphan-deletion path (`DeletionPolicy: Orphan`) | ⚠️ | Skips `Initialize`; uses the fields stored by an earlier reconcile, if any |
| Event-handler callbacks without the resource's name | ⚠️ | No identity to bind to |

Entries for deleted resources stay in the map; each holds only the resource's
correlation fields.

## Changes required

### 1. New file: `internal/correlationlog/correlationlog.go`

The initializer, the store and `CorrelatingLogger`. See the file; its tests in
`correlationlog_test.go` cover cross-resource isolation, async callbacks,
malformed annotations and key remapping.

### 2. New file: `config/correlation.go`

`addCorrelationInitializer` prepends the initializer to every resource. It
passes the resource's configured API group and kind (read when the controller
is set up) as the fallback for objects whose `TypeMeta` is empty.

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

Build the logger with `newZapLogger` (`cmd/provider/logger.go`), set it for
malformed-annotation reports, and wrap it:

```diff
+	logFormat = app.Flag("log-format", "Log output format: json or console.").Default("json").Enum("json", "console")
 	...
-	zl := zap.New(zap.UseDevMode(*debug))
+	zl := newZapLogger(*debug, *logFormat, os.Stderr)
 	baseLogger := logging.NewLogrLogger(zl.WithName("provider-openstack"))
+	correlationlog.SetLogger(baseLogger)
 	logr := correlationlog.NewCorrelatingLogger(baseLogger)
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
