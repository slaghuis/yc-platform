// Package config embeds the service's default configuration.
package config

import _ "embed"

// Default is config.default.yaml. Environment references are expanded when
// it is loaded.
//
//go:embed config.default.yaml
var Default []byte
