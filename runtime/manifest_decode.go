package runtime

import (
	"encoding/json"
	"fmt"

	wc "github.com/partite-ai/wacogo"
)

// decodeManifestRecord lifts the WIT `particle:runtime/manifest.particle-manifest`
// record into the [Manifest] Go struct. The shape mirrors what the
// runtimes' get-manifest exports construct from the user's particle
// dict / default export.
//
// The decoder is verbose because every WIT case (records, options,
// variants, enums, lists) needs an explicit unwrap. Compared against
// json.Unmarshal it's a chore — but the typed WIT contract is the
// authoritative cross-language source of truth, so this decoder is
// what enforces the runtime engines never drift from it. The Manifest
// struct's JSON tags then carry the same shape forward into
// manifest.json.
func decodeManifestRecord(rec *wc.ValRecord) (*Manifest, error) {
	m := &Manifest{}
	m.Name = stringField(rec, "name")
	m.Description = stringField(rec, "description")
	m.Version = stringField(rec, "version")

	if v, ok := rec.Field("runtime").(*wc.ValOption); ok && !v.IsNone() {
		if e, ok := v.Val().(*wc.ValEnum); ok {
			m.Runtime = decodeRuntimeKind(e)
		}
	}

	if caps, ok := rec.Field("capabilities").(*wc.ValRecord); ok {
		m.Capabilities = decodeCapabilitySet(caps)
	}

	if list, ok := rec.Field("credentials").(*wc.ValList); ok {
		creds, err := decodeCredentialList(list)
		if err != nil {
			return nil, err
		}
		m.Credentials = creds
	}

	if list, ok := rec.Field("tools").(*wc.ValList); ok {
		tools, err := decodeToolList(list)
		if err != nil {
			return nil, err
		}
		m.Tools = tools
	}

	return m, nil
}

func decodeRuntimeKind(e *wc.ValEnum) RuntimeKind {
	switch e.Discriminant() {
	case 0:
		return RuntimeJS
	case 1:
		return RuntimePython
	case 2:
		return RuntimeWasm
	}
	return ""
}

func decodeCapabilitySet(rec *wc.ValRecord) Capabilities {
	out := Capabilities{}
	if opt, ok := rec.Field("http").(*wc.ValOption); ok && !opt.IsNone() {
		if httpRec, ok := opt.Val().(*wc.ValRecord); ok {
			if list, ok := httpRec.Field("allowed-hosts").(*wc.ValList); ok {
				out.HTTP.AllowedHosts = decodeStringList(list)
			}
		}
	}
	return out
}

func decodeCredentialList(list *wc.ValList) (map[string]Credential, error) {
	out := make(map[string]Credential, list.Len())
	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("credentials[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		name := stringField(rec, "name")
		cred := Credential{
			Required: boolField(rec, "required"),
		}
		if list, ok := rec.Field("hosts").(*wc.ValList); ok {
			cred.Hosts = decodeStringList(list)
		}
		if methods, ok := rec.Field("methods").(*wc.ValList); ok {
			cred.Methods = make(map[string]CredentialMethod, methods.Len())
			for j := 0; j < methods.Len(); j++ {
				mrec, ok := methods.Get(j).(*wc.ValRecord)
				if !ok {
					return nil, fmt.Errorf("credentials[%d].methods[%d] is %T, want *wacogo.ValRecord",
						i, j, methods.Get(j))
				}
				mname := stringField(mrec, "name")
				method, err := decodeCredentialMethod(mrec)
				if err != nil {
					return nil, fmt.Errorf("credentials[%d].methods[%d] (%q): %w", i, j, mname, err)
				}
				cred.Methods[mname] = method
			}
		}
		out[name] = cred
	}
	return out, nil
}

// decodeCredentialMethod reads one credential-method-entry — i.e. a
// record with `name` / `description` / `method` fields. The variant
// case in `method` discriminates between basic / oauth2 / apikey /
// signing-key / raw and the payload (when present) gets unpacked
// into the matching CredentialMethod.<Kind> sub-struct on the Go
// side.
func decodeCredentialMethod(rec *wc.ValRecord) (CredentialMethod, error) {
	out := CredentialMethod{
		Description: stringField(rec, "description"),
	}
	variant, ok := rec.Field("method").(*wc.ValVariant)
	if !ok {
		return out, fmt.Errorf(".method is %T, want *wacogo.ValVariant", rec.Field("method"))
	}
	switch variant.Discriminant() {
	case 0:
		out.Kind = MethodBasic
	case 1:
		out.Kind = MethodOAuth2
		if payload, ok := variant.Val().(*wc.ValRecord); ok {
			oa := &OAuth2Method{
				AuthorizationURL: stringField(payload, "authorization-url"),
				TokenURL:         stringField(payload, "token-url"),
				DeviceAuthURL:    stringField(payload, "device-auth-url"),
			}
			if list, ok := payload.Field("scopes").(*wc.ValList); ok {
				oa.Scopes = decodeStringList(list)
			}
			if list, ok := payload.Field("flows").(*wc.ValList); ok {
				oa.Flows = decodeOAuth2Flows(list)
			}
			out.OAuth2 = oa
		}
	case 2:
		out.Kind = MethodAPIKey
		if payload, ok := variant.Val().(*wc.ValRecord); ok {
			ak := &APIKeyMethod{}
			if locOpt, ok := payload.Field("location").(*wc.ValOption); ok && !locOpt.IsNone() {
				if locRec, ok := locOpt.Val().(*wc.ValRecord); ok {
					ak.Location = decodeAPIKeyLocation(locRec)
				}
			}
			out.APIKey = ak
		}
	case 3:
		out.Kind = MethodSigningKey
		if payload, ok := variant.Val().(*wc.ValRecord); ok {
			sk := &SigningKeyMethod{}
			if e, ok := payload.Field("algorithm").(*wc.ValEnum); ok {
				switch e.Discriminant() {
				case 0:
					sk.Algorithm = SigningHMACSHA256
				case 1:
					sk.Algorithm = SigningHMACSHA512
				}
			}
			out.SigningKey = sk
		}
	case 4:
		out.Kind = MethodRaw
	default:
		return out, fmt.Errorf("unknown credential-method discriminant %d", variant.Discriminant())
	}
	return out, nil
}

func decodeOAuth2Flows(list *wc.ValList) []OAuth2Flow {
	out := make([]OAuth2Flow, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		e, ok := list.Get(i).(*wc.ValEnum)
		if !ok {
			continue
		}
		switch e.Discriminant() {
		case 0:
			out = append(out, OAuth2FlowAuthorizationCode)
		case 1:
			out = append(out, OAuth2FlowAuthorizationCodePKCE)
		case 2:
			out = append(out, OAuth2FlowDeviceCode)
		}
	}
	return out
}

func decodeAPIKeyLocation(rec *wc.ValRecord) *APIKeyLocation {
	loc := &APIKeyLocation{}
	if e, ok := rec.Field("kind").(*wc.ValEnum); ok {
		switch e.Discriminant() {
		case 0:
			loc.Kind = APIKeyLocationHeader
		case 1:
			loc.Kind = APIKeyLocationAuthScheme
		case 2:
			loc.Kind = APIKeyLocationQueryParam
		}
	}
	if opt, ok := rec.Field("name").(*wc.ValOption); ok && !opt.IsNone() {
		if s, ok := opt.Val().(wc.ValString); ok {
			loc.Name = string(s)
		}
	}
	if opt, ok := rec.Field("scheme").(*wc.ValOption); ok && !opt.IsNone() {
		if s, ok := opt.Val().(wc.ValString); ok {
			loc.Scheme = string(s)
		}
	}
	return loc
}

func decodeToolList(list *wc.ValList) ([]ManifestTool, error) {
	out := make([]ManifestTool, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		rec, ok := list.Get(i).(*wc.ValRecord)
		if !ok {
			return nil, fmt.Errorf("tools[%d] is %T, want *wacogo.ValRecord", i, list.Get(i))
		}
		out = append(out, ManifestTool{
			Name:        stringField(rec, "name"),
			Description: stringField(rec, "description"),
			InputSchema: json.RawMessage(stringField(rec, "input-schema-json")),
		})
	}
	return out, nil
}

func decodeStringList(list *wc.ValList) []string {
	out := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		if s, ok := list.Get(i).(wc.ValString); ok {
			out = append(out, string(s))
		}
	}
	return out
}

func stringField(rec *wc.ValRecord, name string) string {
	if s, ok := rec.Field(name).(wc.ValString); ok {
		return string(s)
	}
	return ""
}

func boolField(rec *wc.ValRecord, name string) bool {
	if b, ok := rec.Field(name).(wc.ValBool); ok {
		return bool(b)
	}
	return false
}
