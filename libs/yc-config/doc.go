// Package config loads layered service configuration: an embedded
// config.default.yaml, an optional override file, then environment
// variables. ${VAR} references in YAML are expanded from the environment.
//
//	//go:embed config.default.yaml
//	var defaultYAML []byte
//
//	var cfg app.Config
//	err := config.Load(&cfg,
//	    config.Bytes("config.default.yaml", defaultYAML),
//	    config.OptionalFile(os.Getenv("YC_CONFIG_FILE")),
//	    config.Env("YC_IDENTITY"),
//	)
package config
