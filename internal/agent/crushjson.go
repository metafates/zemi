// crushjson.go imports provider definitions from a crush.json file so
// zemi works out of the box with keys the user already defined for Crush
// (config key providers.crush_json; DESIGN §6.3). It is a one-way import:
// supported provider kinds become registry specs, everything else is
// skipped with a warning. API keys stay in Go.
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// CrushImport reports which providers were imported and which were
// skipped (with reasons). Warnings never contain key material.
type CrushImport struct {
	Imported []string
	Warnings []string
}

type crushFile struct {
	Providers map[string]crushProvider `json:"providers"`
}

type crushProvider struct {
	Type    string        `json:"type"`
	BaseURL string        `json:"base_url"`
	APIKey  string        `json:"api_key"`
	Models  []crushModel  `json:"models"`
}

type crushModel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ImportCrushJSON parses crush.json content and registers every provider
// whose kind zemi supports. Already-registered ids are skipped so the
// import never fights zemi.provider.register. Malformed JSON is an error;
// per-provider problems become warnings.
func (r *Registry) ImportCrushJSON(data []byte) (CrushImport, error) {
	var file crushFile
	if err := json.Unmarshal(data, &file); err != nil {
		return CrushImport{}, fmt.Errorf("crush.json: %w", err)
	}
	res := CrushImport{}
	for _, id := range sortedKeys(file.Providers) {
		p := file.Providers[id]
		kind := crushKind(p.Type)
		if kind == "" {
			res.warn("provider %s: unsupported type %q, skipped", id, p.Type)
			continue
		}
		spec := Spec{ID: id, Kind: kind, BaseURL: p.BaseURL}
		for _, m := range p.Models {
			if m.ID == "" {
				res.warn("provider %s: model without id, skipped", id)
				continue
			}
			display := m.Name
			if display == "" {
				display = m.ID
			}
			spec.Models = append(spec.Models, Model{ID: m.ID, Display: display})
		}
		if len(spec.Models) == 0 {
			res.warn("provider %s: no models, skipped", id)
			continue
		}
		key, kerr := crushKey(p.APIKey)
		if kerr != "" {
			res.warn("provider %s: %s", id, kerr)
		}
		if err := r.RegisterKeyed(spec, key); err != nil {
			res.warn("provider %s: %v", id, err)
			continue
		}
		res.Imported = append(res.Imported, id)
	}
	return res, nil
}

func (res *CrushImport) warn(format string, args ...any) {
	res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// crushKind maps crush provider types onto zemi kinds; "" means unknown.
func crushKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	case "openai-compat", "openai_compat", "openaicompat":
		return "openaicompat"
	default:
		return ""
	}
}

// crushKey resolves a crush api_key value: "${VAR}" and "env:VAR" read
// the environment, anything else is a literal. Missing env vars yield an
// empty key plus a reason.
func crushKey(v string) (key, problem string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	name := ""
	switch {
	case strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}"):
		name = strings.TrimSuffix(strings.TrimPrefix(v, "${"), "}")
	case strings.HasPrefix(v, "env:"):
		name = strings.TrimPrefix(v, "env:")
	}
	if name == "" {
		return v, ""
	}
	key = os.Getenv(name)
	if key == "" {
		return "", "env var " + name + " is not set"
	}
	return key, ""
}

// ImportCrushFile reads path and imports its providers. A missing file is
// not an error (the knob just has nothing to do).
func (r *Registry) ImportCrushFile(path string) (CrushImport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CrushImport{}, nil
		}
		return CrushImport{}, err
	}
	return r.ImportCrushJSON(data)
}
