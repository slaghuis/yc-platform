package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// Source is one configuration layer. Later sources override earlier ones.
type Source interface {
	load(k *koanf.Koanf) error
}

// Validator is implemented by config structs that check themselves after
// loading. Load calls it.
type Validator interface {
	Validate() error
}

// Load merges sources in order and decodes the result into dst, a pointer
// to a struct with `koanf` tags. Fields absent from every source keep the
// value dst already has, so callers set defaults before calling Load.
//
// Durations decode from strings such as "30s"; numbers and booleans decode
// from strings, so environment overrides work for any field type.
//
//	cfg := Config{HTTP: HTTPConfig{Listen: ":8080"}}
//	err := config.Load(&cfg,
//	    config.Bytes("default", defaultYAML),
//	    config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
//	    config.Env("YC_IDENTITY"),
//	)
func Load(dst any, sources ...Source) error {
	k := koanf.New(".")
	for _, s := range sources {
		if err := s.load(k); err != nil {
			return err
		}
	}
	if err := k.UnmarshalWithConf("", dst, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return fmt.Errorf("config: decode: %w", err)
	}
	if v, ok := dst.(Validator); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	return nil
}

type bytesSource struct {
	name string
	b    []byte
}

// Bytes is a YAML document, typically an embedded config.default.yaml.
// ${VAR} and ${VAR:-default} references are expanded from the environment
// before parsing.
func Bytes(name string, b []byte) Source { return bytesSource{name: name, b: b} }

func (s bytesSource) load(k *koanf.Koanf) error {
	if err := k.Load(rawbytes.Provider(ExpandEnv(s.b)), yaml.Parser()); err != nil {
		return fmt.Errorf("config: parse %s: %w", s.name, err)
	}
	return nil
}

type fileSource struct {
	path     string
	optional bool
}

// File is a YAML file that must exist. Expansion works as for Bytes.
func File(path string) Source { return fileSource{path: path} }

// OptionalFile is a YAML file that is skipped when path is empty or the
// file does not exist.
func OptionalFile(path string) Source { return fileSource{path: path, optional: true} }

func (s fileSource) load(k *koanf.Koanf) error {
	if s.path == "" && s.optional {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if s.optional && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("config: read %s: %w", s.path, err)
	}
	return bytesSource{name: s.path, b: b}.load(k)
}

type envSource struct {
	prefix string
}

// Env overrides individual keys from environment variables named
// <PREFIX>__<KEY>__<SUBKEY>, for example YC_GATEWAY__HTTP__LISTEN=:9090
// sets http.listen. Double underscores separate levels so keys may contain
// single underscores (YC_GATEWAY__TELEMETRY__OTLP_ENDPOINT).
func Env(prefix string) Source { return envSource{prefix: strings.TrimSuffix(prefix, "_") + "__"} }

func (s envSource) load(k *koanf.Koanf) error {
	p := env.Provider(s.prefix, ".", func(key string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, s.prefix), "__", "."))
	})
	if err := k.Load(p, nil); err != nil {
		return fmt.Errorf("config: env %s: %w", s.prefix, err)
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} with the variable's value (empty when unset)
// and ${VAR:-default} with the value, or default when unset or empty. A bare
// $ is left alone, so values such as passwords may contain it.
func ExpandEnv(b []byte) []byte {
	return envRef.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := envRef.FindSubmatch(m)
		if v := os.Getenv(string(sub[1])); v != "" {
			return []byte(v)
		}
		return sub[3]
	})
}
