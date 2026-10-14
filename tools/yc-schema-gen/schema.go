package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const idPrefix = "https://yc/schemas/"

// Schema is the subset of JSON Schema 2020-12 the generator understands.
// Validation keywords it ignores (pattern, minimum, …) are still enforced
// at runtime by the validator in yc-events.
type Schema struct {
	ID          string     `json:"$id"`
	Ref         string     `json:"$ref"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Type        typeList   `json:"type"`
	Format      string     `json:"format"`
	Enum        []string   `json:"enum"`
	Required    []string   `json:"required"`
	Properties  properties `json:"properties"`
	Items       *Schema    `json:"items"`
}

// typeList accepts "type": "string" and "type": ["string", "null"].
type typeList []string

func (t *typeList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = typeList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*t = many
	return nil
}

// nonNull returns the single non-null type and whether null is allowed.
// It returns "" when the schema has no type or several non-null types.
func (t typeList) nonNull() (string, bool) {
	var types []string
	nullable := false
	for _, s := range t {
		if s == "null" {
			nullable = true
			continue
		}
		types = append(types, s)
	}
	if len(types) != 1 {
		return "", nullable
	}
	return types[0], nullable
}

// properties keeps the schema's property order so generated structs read
// like the schema.
type properties struct {
	keys []string
	m    map[string]*Schema
}

func (p *properties) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("properties must be an object")
	}
	p.m = map[string]*Schema{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		var s Schema
		if err := dec.Decode(&s); err != nil {
			return fmt.Errorf("property %q: %w", key, err)
		}
		p.keys = append(p.keys, key)
		p.m[key] = &s
	}
	return nil
}

type fileKind int

const (
	kindEnvelope fileKind = iota
	kindCommon
	kindEvent
)

type schemaFile struct {
	rel    string // slash-separated, relative to the schema root
	kind   fileKind
	raw    []byte
	schema *Schema
	// eventType is set for events, e.g. "identity.visitor.created.v1".
	eventType string
}

// loadSchemas reads every *.json under dir, sorted by path, and checks that
// file names and $id values follow the repository's layout.
func loadSchemas(dir string) ([]schemaFile, error) {
	var files []schemaFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".json" {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		f, err := loadSchema(p, filepath.ToSlash(rel))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func loadSchema(p, rel string) (schemaFile, error) {
	raw, err := os.ReadFile(p) //nolint:gosec // paths come from walking the schema directory
	if err != nil {
		return schemaFile{}, err
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return schemaFile{}, err
	}
	if want := idPrefix + rel; s.ID != want {
		return schemaFile{}, fmt.Errorf("$id is %q, want %q", s.ID, want)
	}

	f := schemaFile{rel: rel, raw: raw, schema: &s}
	dir, name := path.Split(rel)
	switch dir {
	case "envelope/":
		f.kind = kindEnvelope
	case "common/":
		f.kind = kindCommon
	default:
		domain := strings.TrimSuffix(dir, "/")
		f.kind = kindEvent
		f.eventType = strings.TrimSuffix(name, ".json")
		if !strings.HasPrefix(f.eventType, domain+".") {
			return schemaFile{}, fmt.Errorf("event schema must be named <domain>/<domain>.….json")
		}
		if s.Title != f.eventType {
			return schemaFile{}, fmt.Errorf("title is %q, want %q", s.Title, f.eventType)
		}
	}
	if f.kind != kindEnvelope {
		if t, _ := s.Type.nonNull(); t != "object" {
			return schemaFile{}, fmt.Errorf("top-level type must be object")
		}
	}
	return f, nil
}
