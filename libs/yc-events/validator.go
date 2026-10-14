package events

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	eventbus "github.com/slaghuis/yc-platform/libs/yc-eventbus"
)

//go:embed schemas
var schemaFS embed.FS

const (
	idPrefix   = "https://yc/schemas/"
	envelopeID = idPrefix + "envelope/v1.json"
)

// Validator checks envelopes and payloads against the embedded schemas,
// including formats (uuid, date-time, email). It implements
// eventbus.Validator; pass it as eventbus.Config.Validator.
type Validator struct {
	envelope *jsonschema.Schema
	payloads map[string]*jsonschema.Schema
}

var _ eventbus.Validator = (*Validator)(nil)

// NewValidator compiles every embedded schema.
func NewValidator() (*Validator, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()

	var eventFiles []string
	err := fs.WalkDir(schemaFS, "schemas", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		raw, err := schemaFS.ReadFile(p)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		rel := strings.TrimPrefix(p, "schemas/")
		if err := c.AddResource(idPrefix+rel, doc); err != nil {
			return err
		}
		if dir := path.Dir(rel); dir != "envelope" && dir != "common" {
			eventFiles = append(eventFiles, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("events: load schemas: %w", err)
	}

	v := &Validator{payloads: make(map[string]*jsonschema.Schema, len(eventFiles))}
	if v.envelope, err = c.Compile(envelopeID); err != nil {
		return nil, fmt.Errorf("events: compile envelope: %w", err)
	}
	for _, rel := range eventFiles {
		s, err := c.Compile(idPrefix + rel)
		if err != nil {
			return nil, fmt.Errorf("events: compile %s: %w", rel, err)
		}
		v.payloads[strings.TrimSuffix(path.Base(rel), ".json")] = s
	}
	return v, nil
}

// ValidateEnvelope checks the envelope's structure; the payload only has to
// be an object here.
func (v *Validator) ValidateEnvelope(_ context.Context, env *eventbus.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("events: marshal envelope: %w", err)
	}
	return validate(v.envelope, b, "envelope")
}

// ValidatePayload checks the payload against the schema for env.EventType.
// An event type without a schema is rejected.
func (v *Validator) ValidatePayload(_ context.Context, env *eventbus.Envelope) error {
	s, ok := v.payloads[env.EventType]
	if !ok {
		return fmt.Errorf("events: no schema for event type %q", env.EventType)
	}
	return validate(s, env.Payload, env.EventType)
}

func validate(s *jsonschema.Schema, data []byte, what string) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("events: %s is not JSON: %w", what, err)
	}
	if err := s.Validate(inst); err != nil {
		return fmt.Errorf("events: %s: %w", what, err)
	}
	return nil
}
