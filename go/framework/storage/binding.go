package storage

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"os"
	"strings"

	"go.putnami.dev/errors"
	diag "go.putnami.dev/protocol/diagnostic"
	storageproto "go.putnami.dev/protocol/storage"
)

// EnvBindings is the environment variable a deploy target injects to bind every
// logical bucket a workload registered to the provider bucket the control plane
// provisioned for it: a workload that added storage.NewPlugin() resolves a
// working, correctly-bound storage.Backend from it with no bespoke per-workload
// bucket env. It is the legacy transport for the managed bindings document:
// deploy targets now merge the same document into the "storage" section of the
// workload's resolved config (see ConfigSection), which wins when both are
// present; the env var remains the fallback until every deploy target stops
// injecting it. The protocol/storage package owns the per-binding shape
// (storageproto.Binding); this package owns the document encoding the deployer
// injects, which protocol/storage deliberately leaves to the runtime.
//
// The value is a BindingsDocument encoded as JSON (see BindingsDocument).
const EnvBindings = "STORAGE_BINDINGS"

// BindingsProtocolVersion is the version of the STORAGE_BINDINGS envelope this
// runtime accepts. It is independent of storageproto.ProtocolVersion (the
// per-binding shape): the envelope can evolve without bumping the binding
// contract and vice versa. Bumped on any backwards-incompatible change to the
// envelope shape.
const BindingsProtocolVersion = 1

// Backend kinds a binding may select. Kept provider-agnostic: the binding layer
// only remaps names and delegates, so adding a kind is a matter of teaching
// backendForKind how to construct it.
const (
	backendKindMemory = "memory"
	backendKindGCS    = "gcs"
	backendKindS3     = "s3"
)

// BindingsDocument is the JSON document a deploy target injects via the
// STORAGE_BINDINGS environment variable.
//
//	{
//	  "$schema": "https://putnami.dev/schemas/putnami-storage-bindings.json",
//	  "protocolVersion": 1,
//	  "bindings": [
//	    { "name": "uploads", "backend": "gcs", "bucket": "pn-acme-prod-uploads-a1b2c3", "identity": "workload", "signedUrls": true },
//	    { "name": "exports", "backend": "gcs", "bucket": "acme-shared-prod", "prefix": "preview-42/exports/" }
//	  ],
//	  "providers": { "gcs": { "projectId": "acme-prod" } }
//	}
//
// Each Bindings entry is a storageproto.Binding: name is the logical bucket the
// workload registered, bucket is the provisioned provider bucket, and prefix is
// an optional key prefix when several logical buckets share one provider bucket.
// Providers carries the per-backend construction parameters a Binding does not
// itself convey (GCP project, S3 region/endpoint, ...).
type BindingsDocument struct {
	Schema          string                 `json:"$schema,omitempty"`
	ProtocolVersion int                    `json:"protocolVersion"`
	Bindings        []storageproto.Binding `json:"bindings"`
	Providers       ProviderParams         `json:"providers,omitempty"`
}

// ProviderParams carries per-backend-kind construction parameters that a
// storageproto.Binding does not convey. A workload-identity GCS deploy (the
// common case) needs none of these: bucket names are globally unique and the
// metadata server supplies credentials.
type ProviderParams struct {
	GCS *GCSParams `json:"gcs,omitempty"`
	S3  *S3Params  `json:"s3,omitempty"`
}

// GCSParams configures the GCS backend a gcs binding resolves to.
type GCSParams struct {
	ProjectID       string `json:"projectId,omitempty"`
	CredentialsFile string `json:"credentialsFile,omitempty"`
}

// S3Params configures the S3 backend an s3 binding resolves to. AccessKey and
// SecretKey are sensitive; prefer workload identity / GCS where no static
// secret is needed.
type S3Params struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty"`
}

// ParseBindings strictly decodes and validates a BindingsDocument. Unknown
// fields are rejected so a typo in the injected contract fails loudly rather
// than silently dropping a binding. Each entry is validated against the
// protocol/storage binding rules (canonical name, non-empty backend/bucket,
// in-enum identity); a per-binding protocolVersion is defaulted to the current
// protocol version when omitted so a deployer need not repeat it on every entry.
func ParseBindings(data []byte) (*BindingsDocument, error) {
	return parseBindingsDocument(data, errors.String("env", EnvBindings))
}

// parseBindingsDocument is ParseBindings with the document's transport named
// in diagnostics: the EnvBindings env var or the resolved-config section (see
// ConfigSection). Both transports share this decode + validation so
// misconfiguration reads identically wherever the document came from.
func parseBindingsDocument(data []byte, source errors.Attr) (*BindingsDocument, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc BindingsDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("op", "parse_bindings"), source)
	}
	if err := rejectTrailingJSON(dec); err != nil {
		return nil, errors.Wrap(err, CodeStorageRequest,
			errors.String("op", "parse_bindings"), source)
	}

	if err := validateBindingsDocument(&doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func rejectTrailingJSON(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	return stderrors.New("unexpected trailing JSON value")
}

func validateBindingsDocument(doc *BindingsDocument) error {
	if doc == nil {
		return errors.New(CodeStorageRequest, "nil storage bindings document")
	}
	if doc.ProtocolVersion != BindingsProtocolVersion {
		return errors.New(CodeStorageRequest, "unsupported STORAGE_BINDINGS protocolVersion",
			errors.Int("protocolVersion", doc.ProtocolVersion), errors.Int("supported", BindingsProtocolVersion))
	}
	if len(doc.Bindings) == 0 {
		return errors.New(CodeStorageRequest, "STORAGE_BINDINGS contains no bindings",
			errors.String("env", EnvBindings))
	}

	seen := make(map[string]bool, len(doc.Bindings))
	var diags []diag.Diagnostic
	for i := range doc.Bindings {
		b := &doc.Bindings[i]
		if b.ProtocolVersion == 0 {
			b.ProtocolVersion = storageproto.ProtocolVersion
		}
		diags = append(diags, storageproto.ValidateBinding(b)...)
		if seen[b.Name] {
			return errors.New(CodeStorageRequest, "duplicate logical bucket in STORAGE_BINDINGS",
				errors.String("bucket", b.Name))
		}
		seen[b.Name] = true
		if !supportedBackendKind(b.Backend) {
			return errors.New(CodeStorageRequest, "unsupported storage backend kind in STORAGE_BINDINGS",
				errors.String("bucket", b.Name), errors.String("backend", b.Backend),
				errors.String("supported", strings.Join([]string{backendKindMemory, backendKindGCS, backendKindS3}, ", ")))
		}
	}
	if diag.HasErrors(diags) {
		return errors.New(CodeStorageRequest, "invalid STORAGE_BINDINGS:\n"+diag.ErrorText(diags))
	}
	return nil
}

func supportedBackendKind(kind string) bool {
	switch kind {
	case backendKindMemory, backendKindGCS, backendKindS3:
		return true
	default:
		return false
	}
}

// HasManagedBindings reports whether a managed storage bindings document is
// resolvable for this process — from the "storage" config section (see
// ConfigSection) or, during transition, the STORAGE_BINDINGS env fallback (see
// EnvBindings). It does NOT construct a backend or resolve a specific logical
// bucket; it answers the whole-process "is this process bound to managed storage
// at all" question a deploy target's composition uses to mount the
// binding-resolved backend over a local config-driven one before any plugin is
// mounted.
//
// It is transport-agnostic: the config section wins, the env var is the fallback
// until every deploy target stops injecting it. It reuses loadConfigBindings —
// the same internal resolution the plugin applies at Configure — so a present-
// but-malformed section reports true (loadConfigBindings returns an error, never
// a silent nil) and then fails loudly at mount, exactly as the plugin's own
// resolution does; the predicate can never disagree with the document the plugin
// resolves. A section carrying no document falls through to the env presence
// check, and neither transport present is false, so local serve/test/describe
// keep their local/memory path unchanged.
func HasManagedBindings(ctx context.Context) bool {
	doc, err := loadConfigBindings(ctx)
	if err != nil || doc != nil {
		return true
	}
	return strings.TrimSpace(os.Getenv(EnvBindings)) != ""
}

// DiscoverBackend builds a bound Backend from the STORAGE_BINDINGS environment
// variable. It returns (nil, nil) when the variable is unset or blank so a
// caller can fall back to a manually constructed backend, an error when the
// injected document is malformed, and a ready *bindingBackend otherwise.
func DiscoverBackend(ctx context.Context) (Backend, error) {
	raw := strings.TrimSpace(os.Getenv(EnvBindings))
	if raw == "" {
		return nil, nil
	}
	doc, err := ParseBindings([]byte(raw))
	if err != nil {
		return nil, err
	}
	return BackendFromBindings(ctx, doc)
}

// BackendFromBindings constructs a Backend that resolves the logical buckets in
// doc to their bound provider buckets. One underlying backend is constructed per
// referenced kind (a single GCS backend serves every gcs binding), wrapped in a
// remap layer that translates each logical bucket — and the optional per-binding
// key prefix — on every operation. Construction is cheap: backends defer network
// work (token fetches, connections) until first use.
func BackendFromBindings(ctx context.Context, doc *BindingsDocument) (Backend, error) {
	if err := validateBindingsDocument(doc); err != nil {
		return nil, err
	}

	byKind := make(map[string]Backend)
	var order []Backend
	ensure := func(kind string) (Backend, error) {
		if bk, ok := byKind[kind]; ok {
			return bk, nil
		}
		bk, err := backendForKind(ctx, kind, doc.Providers)
		if err != nil {
			return nil, err
		}
		byKind[kind] = bk
		order = append(order, bk)
		return bk, nil
	}

	targets := make(map[string]boundTarget, len(doc.Bindings))
	for i := range doc.Bindings {
		b := doc.Bindings[i]
		bk, err := ensure(b.Backend)
		if err != nil {
			closeAll(order)
			return nil, err
		}
		targets[b.Name] = boundTarget{backend: bk, bucket: b.Bucket, prefix: b.Prefix, signedURLs: b.SignedUrls}
	}
	return &bindingBackend{targets: targets, backends: order}, nil
}

// backendForKind constructs the underlying backend for a binding kind using the
// matching provider params. Nil params are valid: a gcs backend then uses
// Application Default Credentials, which is the workload-identity default.
func backendForKind(ctx context.Context, kind string, providers ProviderParams) (Backend, error) {
	switch kind {
	case backendKindMemory:
		return NewMemoryBackend(), nil
	case backendKindGCS:
		cfg := GCSConfig{}
		if providers.GCS != nil {
			cfg.ProjectID = providers.GCS.ProjectID
			cfg.CredentialsFile = providers.GCS.CredentialsFile
		}
		return NewGCSBackend(ctx, cfg)
	case backendKindS3:
		cfg := S3Config{}
		if providers.S3 != nil {
			cfg = S3Config{
				Endpoint:  providers.S3.Endpoint,
				Region:    providers.S3.Region,
				AccessKey: providers.S3.AccessKey,
				SecretKey: providers.S3.SecretKey,
			}
		}
		return NewS3Backend(cfg), nil
	default:
		return nil, errors.New(CodeStorageRequest, "unsupported storage backend kind",
			errors.String("backend", kind))
	}
}

func closeAll(backends []Backend) {
	for _, bk := range backends {
		_ = bk.Close() //nolint:errcheck // best-effort cleanup while unwinding a failed construction
	}
}

// boundTarget is the resolved provider coordinates for one logical bucket.
type boundTarget struct {
	backend    Backend
	bucket     string // provider bucket the logical name resolves to
	prefix     string // key prefix prepended within bucket (empty for a dedicated bucket)
	signedURLs bool   // whether the deployer granted signed-URL capability
}

// bindingBackend is a remap layer over one or more concrete backends. Each
// operation resolves the caller's logical bucket to the provider bucket it was
// bound to, prepends the binding's key prefix, and delegates. A logical bucket
// with no binding fails closed (CodeStorageUnbound) — it is never silently sent
// to a literal-named bucket.
//
// The remap is provider-agnostic: every backend treats its bucket argument as
// the provider's own identifier, so swapping the logical name for the provider
// name is all the layer does. It implements URLSigner so signed URLs keep
// working behind the binding layer when the resolved backend supports them.
type bindingBackend struct {
	targets  map[string]boundTarget
	backends []Backend // distinct underlying backends, for Close
}

// Ensure bindingBackend implements every interface at compile time.
var (
	_ Backend   = (*bindingBackend)(nil)
	_ Stater    = (*bindingBackend)(nil)
	_ URLSigner = (*bindingBackend)(nil)
)

// resolve maps a logical bucket to its bound provider coordinates, failing
// closed when the logical bucket has no injected binding.
func (b *bindingBackend) resolve(logical string) (boundTarget, error) {
	t, ok := b.targets[logical]
	if !ok {
		return boundTarget{}, errors.New(CodeStorageUnbound,
			"no storage binding for logical bucket: the deploy target must inject a "+EnvBindings+" entry for it",
			errors.String("backend", "binding"), errors.String("bucket", logical))
	}
	return t, nil
}

// Put delegates to the bound backend under the provider bucket and prefixed key,
// then restores the logical key on the result.
func (b *bindingBackend) Put(ctx context.Context, bucket, key string, data io.Reader, meta *ObjectMetadata) (*PutResult, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return nil, err
	}
	res, err := t.backend.Put(ctx, t.bucket, t.prefix+key, data, meta)
	if res != nil && t.prefix != "" {
		res.Key = strings.TrimPrefix(res.Key, t.prefix)
	}
	return res, err
}

// Get delegates to the bound backend and restores the logical key on the result.
func (b *bindingBackend) Get(ctx context.Context, bucket, key string) (*GetResult, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return nil, err
	}
	res, err := t.backend.Get(ctx, t.bucket, t.prefix+key)
	if res != nil && t.prefix != "" {
		res.Key = strings.TrimPrefix(res.Key, t.prefix)
	}
	return res, err
}

// Delete delegates to the bound backend under the provider bucket and prefixed key.
func (b *bindingBackend) Delete(ctx context.Context, bucket, key string) error {
	t, err := b.resolve(bucket)
	if err != nil {
		return err
	}
	return t.backend.Delete(ctx, t.bucket, t.prefix+key)
}

// Exists delegates to the bound backend under the provider bucket and prefixed key.
func (b *bindingBackend) Exists(ctx context.Context, bucket, key string) (bool, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return false, err
	}
	return t.backend.Exists(ctx, t.bucket, t.prefix+key)
}

// Stat delegates to the bound backend under the provider bucket and prefixed
// key, then restores the logical key on the result. It fails with
// CodeStorageUnsupported when the bound backend does not implement Stater.
func (b *bindingBackend) Stat(ctx context.Context, bucket, key string) (*ObjectInfo, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return nil, err
	}
	res, err := Stat(ctx, t.backend, t.bucket, t.prefix+key)
	if res != nil && t.prefix != "" {
		res.Key = strings.TrimPrefix(res.Key, t.prefix)
	}
	return res, err
}

// Copy delegates to the bound backend, prefixing both source and destination.
func (b *bindingBackend) Copy(ctx context.Context, bucket, source, destination string) error {
	t, err := b.resolve(bucket)
	if err != nil {
		return err
	}
	return t.backend.Copy(ctx, t.bucket, t.prefix+source, t.prefix+destination)
}

// List delegates to the bound backend, scoping the listing to the binding's key
// prefix and stripping that prefix from the returned keys and common prefixes so
// the caller sees logical keys. The continuation token is left untouched: it is
// opaque to callers and round-trips back to the same backend, so prefixing it
// would corrupt providers (e.g. S3) whose token is not a key.
func (b *bindingBackend) List(ctx context.Context, bucket string, opts *ListOptions) (*ListResult, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return nil, err
	}
	eff := opts
	if t.prefix != "" {
		cp := ListOptions{}
		if opts != nil {
			cp = *opts
		}
		cp.Prefix = t.prefix + cp.Prefix
		eff = &cp
	}
	res, err := t.backend.List(ctx, t.bucket, eff)
	if err != nil || res == nil || t.prefix == "" {
		return res, err
	}
	for i := range res.Objects {
		res.Objects[i].Key = strings.TrimPrefix(res.Objects[i].Key, t.prefix)
	}
	for i := range res.Prefixes {
		res.Prefixes[i] = strings.TrimPrefix(res.Prefixes[i], t.prefix)
	}
	return res, nil
}

// SignedGetURL issues a pre-signed GET URL for the bound provider object when
// the resolved backend is a URLSigner; otherwise it fails with
// CodeStorageUnsupported rather than silently returning nothing.
func (b *bindingBackend) SignedGetURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	t, signer, err := b.resolveSigner(bucket)
	if err != nil {
		return "", err
	}
	return signer.SignedGetURL(ctx, t.bucket, t.prefix+key, opts)
}

// SignedPutURL issues a pre-signed PUT URL for the bound provider object when
// the resolved backend is a URLSigner.
func (b *bindingBackend) SignedPutURL(ctx context.Context, bucket, key string, opts SignedURLOptions) (string, error) {
	t, signer, err := b.resolveSigner(bucket)
	if err != nil {
		return "", err
	}
	return signer.SignedPutURL(ctx, t.bucket, t.prefix+key, opts)
}

// resolveSigner resolves a logical bucket and asserts its backend can sign URLs.
func (b *bindingBackend) resolveSigner(bucket string) (boundTarget, URLSigner, error) {
	t, err := b.resolve(bucket)
	if err != nil {
		return boundTarget{}, nil, err
	}
	if !t.signedURLs {
		return boundTarget{}, nil, errors.New(CodeStorageUnsupported,
			"storage binding does not grant signed URLs",
			errors.String("backend", "binding"), errors.String("bucket", bucket),
			errors.Bool("signedUrls", false))
	}
	signer, ok := t.backend.(URLSigner)
	if !ok {
		return boundTarget{}, nil, errors.New(CodeStorageUnsupported,
			"resolved backend does not support signed URLs",
			errors.String("backend", "binding"), errors.String("bucket", bucket))
	}
	return t, signer, nil
}

// Close closes every distinct underlying backend, joining any errors.
func (b *bindingBackend) Close() error {
	var errs []error
	for _, bk := range b.backends {
		if err := bk.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return stderrors.Join(errs...)
}
