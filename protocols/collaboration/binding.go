package collaboration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// BindingOptionNamespace is where a workspace binds contracts to providers:
// the options.collaboration block of the workspace document
// (putnami.workspace.json), and nowhere else. A project configuration or the
// user's global configuration never binds a provider.
const BindingOptionNamespace = "collaboration"

// MaxSettingsBytes bounds a binding's settings object.
const MaxSettingsBytes = 16 << 10

// Binding binds one contract to one provider extension.
type Binding struct {
	// Provider is the provider extension's name as the workspace resolves it
	// (its manifest name). Exactly one available extension must carry it.
	Provider string `json:"provider"`
	// Version is the contract version the workspace speaks. The provider must
	// implement it; the orchestrator never substitutes another.
	Version int `json:"version"`
	// Require lists optional operations the workspace depends on. A provider
	// that lacks one makes the binding invalid instead of failing the first
	// call that needs it.
	Require []string `json:"require,omitempty"`
	// Settings is the provider's own configuration, passed verbatim on every
	// call. It never carries a credential: a provider resolves its own.
	Settings json.RawMessage `json:"settings,omitempty"`
}

// Bindings maps contract names to their binding.
type Bindings map[string]Binding

// Contracts returns the bound contract names in canonical order.
func (b Bindings) Contracts() []string {
	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseBindings strictly decodes and validates an options.collaboration
// block. A repeated member, an unknown contract or member, an unsupported
// version, an unknown required operation and a credential-looking setting are
// all errors: a binding that could be read two ways is refused rather than
// guessed.
func ParseBindings(data []byte) (Bindings, []diag.Diagnostic) {
	if len(data) > MaxDocumentBytes {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeTooLarge, "", "the binding block exceeds %d bytes", MaxDocumentBytes)}
	}
	if field, found := firstDuplicateMember(data); found {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeDuplicateMember, field,
			"%s is bound more than once; the binding is ambiguous", field)}
	}
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "options.%s must be an object", BindingOptionNamespace)}
	}
	contracts := make([]string, 0, len(raw))
	for name := range raw {
		contracts = append(contracts, name)
	}
	sort.Strings(contracts)

	bindings := Bindings{}
	var diags []diag.Diagnostic
	for _, contract := range contracts {
		if !IsContract(contract) {
			diags = append(diags, diag.Errorf(ErrorCodeUnknownContract, contract,
				"unknown contract %q (contracts: %s)", contract, strings.Join(ContractNames, ", ")))
			continue
		}
		var binding Binding
		if decodeDiags := strictDecode(raw[contract], &binding); decodeDiags != nil {
			diags = append(diags, prefixDiagnostics(contract, decodeDiags)...)
			continue
		}
		bindingDiags := ValidateBinding(contract, binding)
		diags = append(diags, bindingDiags...)
		if !diag.HasErrors(bindingDiags) {
			bindings[contract] = binding
		}
	}
	if diag.HasErrors(diags) {
		return bindings, diags
	}
	return bindings, nil
}

// ValidateBinding checks one contract's binding.
func ValidateBinding(contract string, binding Binding) []diag.Diagnostic {
	c := &checker{}
	provider := strings.TrimSpace(binding.Provider)
	switch {
	case provider == "":
		c.errorf(ErrorCodeRequired, contract+".provider", "a binding names its provider extension")
	case provider != binding.Provider || len(provider) > 214:
		c.errorf(ErrorCodeInvalidValue, contract+".provider", "provider must be an extension name of at most 214 characters")
	}
	spec, supported := Lookup(contract, binding.Version)
	switch {
	case binding.Version < 1:
		c.errorf(ErrorCodeRequired, contract+".version", "a binding names the contract version it speaks")
	case !supported:
		c.errorf(ErrorCodeUnsupportedVersion, contract+".version",
			"%s version %d is not supported by this putnami (supported: %s)",
			contract, binding.Version, formatVersions(SupportedVersions(contract)))
	}
	seen := map[string]bool{}
	for i, name := range binding.Require {
		field := fmt.Sprintf("%s.require[%d]", contract, i)
		if seen[name] {
			c.errorf(ErrorCodeInvalidValue, field, "operation %q is required twice", name)
			continue
		}
		seen[name] = true
		if !supported {
			continue
		}
		op, ok := spec.Operation(name)
		switch {
		case !ok:
			c.errorf(ErrorCodeUnknownOperation, field, "%s version %d has no operation %q", contract, binding.Version, name)
		case op.Required:
			c.errorf(ErrorCodeInvalidValue, field, "operation %q is already required by the contract", name)
		}
	}
	if len(binding.Settings) > 0 {
		c.settings(contract+".settings", binding.Settings)
	}
	return c.diags
}

// credentialWords are the member-name fragments a binding setting may not
// carry. Settings are committed configuration, echoed by capability discovery;
// a credential in them would be published with the workspace.
var credentialWords = []string{
	"apikey", "authorization", "bearer", "cookie", "credential", "passphrase",
	"passwd", "password", "privatekey", "secret", "token",
}

func (c *checker) settings(field string, raw json.RawMessage) {
	if len(raw) > MaxSettingsBytes {
		c.errorf(ErrorCodeTooLarge, field, "settings exceed %d bytes", MaxSettingsBytes)
		return
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		c.errorf(ErrorCodeParseError, field, "settings are not valid JSON: %v", err)
		return
	}
	if _, ok := value.(map[string]any); !ok {
		c.errorf(ErrorCodeInvalidValue, field, "settings must be an object")
		return
	}
	if path, found := firstCredentialMember(value, field); found {
		c.errorf(ErrorCodeCredentialSetting, path,
			"%s looks like a credential; a binding never carries one: the provider resolves its own credentials", path)
	}
	if path, found := firstCredentialURL(value, field); found {
		c.errorf(ErrorCodeCredentialSetting, path,
			"%s is a URL that carries credentials; a binding never carries one: the provider resolves its own credentials", path)
	}
}

// firstCredentialURL returns the path of the first string value, in sorted
// member order, that is a URL carrying credentials.
func firstCredentialURL(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case string:
		return path, urlCarriesCredentials(typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if found, ok := firstCredentialURL(typed[key], joinPath(path, key)); ok {
				return found, true
			}
		}
	case []any:
		for i, item := range typed {
			if found, ok := firstCredentialURL(item, fmt.Sprintf("%s[%d]", path, i)); ok {
				return found, true
			}
		}
	}
	return "", false
}

// urlCarriesCredentials reports whether value is a URL whose userinfo holds a
// credential: a password under any scheme, or any userinfo under http or
// https, whose user slot carries access tokens. A user without a password
// under another scheme (ssh://git@host/repo.git) names an account, not a
// secret.
func urlCarriesCredentials(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.User == nil {
		return false
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return true
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

func firstCredentialMember(value any, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := joinPath(path, key)
			if IsCredentialName(key) {
				return child, true
			}
			if found, ok := firstCredentialMember(typed[key], child); ok {
				return found, true
			}
		}
	case []any:
		for i, item := range typed {
			if found, ok := firstCredentialMember(item, fmt.Sprintf("%s[%d]", path, i)); ok {
				return found, true
			}
		}
	}
	return "", false
}

// IsCredentialName reports whether a member or variable name looks like it
// holds a credential: it contains, ignoring case and punctuation, one of the
// credential words (token, secret, password, apikey, privatekey,
// authorization, bearer, cookie, credential, passphrase, passwd).
func IsCredentialName(name string) bool {
	var folded strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			folded.WriteRune(r)
		}
	}
	key := folded.String()
	return slices.ContainsFunc(credentialWords, func(word string) bool { return strings.Contains(key, word) })
}
