package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// envRef matches ${NAME} and ${NAME:-default}.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv substitutes ${VAR} and ${VAR:-default} references from the
// environment. A reference with no value and no default expands to empty, which
// validation then reports as a missing required field rather than silently
// passing an empty credential upstream.
func ExpandEnv(in []byte) []byte {
	return envRef.ReplaceAllFunc(in, func(m []byte) []byte {
		groups := envRef.FindSubmatch(m)
		name, fallback := string(groups[1]), string(groups[2])
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return []byte(v)
		}
		return []byte(fallback)
	})
}

// Load reads, expands, parses, defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse handles an in-memory config document. It is the testable core of Load.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(ExpandEnv(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := Finalize(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Finalize applies defaults, derives deployment identifiers and validates a
// Config that was built in memory rather than parsed from YAML. Parse calls it,
// and so must anything that constructs a Config directly: deployment IDs are
// assigned here, and routing state keys on them.
func Finalize(cfg *Config) error {
	cfg.applyDefaults()

	for i := range cfg.ModelList {
		cfg.ModelList[i].setID()
	}
	// The hierarchy is resolved before validation because validation reads it:
	// a key naming a scope is checked against the resolved map rather than
	// against the nesting it came from.
	if err := cfg.resolveScopes(); err != nil {
		return err
	}

	return cfg.Validate()
}
