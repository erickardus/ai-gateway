package config

import (
	"strings"
	"testing"
)

// baseModelList is the smallest configuration that validates, so an audit block
// is the only thing under test.
const baseModelList = `
model_list:
  - model_name: m
    params:
      format: anthropic
      api_base: https://api.anthropic.com
      auth_mode: api_key
      auth_header: x-api-key
      api_key: k
`

func TestAuditConfig(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		wantErr string
		// check runs on a config that parsed, to pin what the defaults produce.
		check func(t *testing.T, c *Config)
	}{
		{
			name: "silence means on, to stdout",
			check: func(t *testing.T, c *Config) {
				if !c.Audit.On() {
					t.Error("auditing is off by default; administrative actions would go unrecorded unless asked for")
				}
				if c.Audit.SinkKind() != AuditSinkStdout {
					t.Errorf("default sink = %q, want %q", c.Audit.SinkKind(), AuditSinkStdout)
				}
			},
		},
		{
			name:  "switched off explicitly",
			block: "audit:\n  enabled: false\n",
			check: func(t *testing.T, c *Config) {
				if c.Audit.On() {
					t.Error("enabled: false left auditing on")
				}
			},
		},
		{
			name:  "a file sink with somewhere to write",
			block: "audit:\n  sink: file\n  path: ./data/audit.jsonl\n",
			check: func(t *testing.T, c *Config) {
				if c.Audit.SinkKind() != AuditSinkFile || c.Audit.Path != "./data/audit.jsonl" {
					t.Errorf("audit = %+v, want the file sink at ./data/audit.jsonl", c.Audit)
				}
			},
		},
		{
			name:    "a file sink with nowhere to write",
			block:   "audit:\n  sink: file\n",
			wantErr: "audit.path: required when audit.sink is \"file\"",
		},
		{
			name:    "a path the stdout sink would ignore",
			block:   "audit:\n  path: ./data/audit.jsonl\n",
			wantErr: "audit.path: \"./data/audit.jsonl\" is set while audit.sink is \"stdout\"",
		},
		{
			name:    "a sink nothing implements",
			block:   "audit:\n  sink: syslog\n",
			wantErr: "audit.sink: must be \"stdout\" or \"file\", got \"syslog\"",
		},
		{
			// A typo is caught whether or not the block is enabled, because the
			// day it is turned on is the day it is needed.
			name:    "a typo in a block that is switched off",
			block:   "audit:\n  enabled: false\n  sink: fille\n",
			wantErr: "audit.sink: must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(baseModelList + tt.block))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Parse: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Parse succeeded, want an error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}
