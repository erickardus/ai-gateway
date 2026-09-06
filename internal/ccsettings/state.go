package ccsettings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateFile is where the login's own record lives, beside the settings it
// wrote. It is separate from settings.json because Claude Code owns that file
// and would have no use for these fields.
const StateFile = ".gateway-sso.json"

// State is what a login remembers so that a later renewal needs no interaction.
//
// The refresh token in here is a bearer credential: whoever holds it can obtain
// a gateway key for this identity until the provider revokes it. It sits at the
// same permissions, in the same directory, as the virtual key it renews, so it
// is not a new class of secret on the machine — but it is the reason this file
// is written the way it is.
type State struct {
	Gateway      string    `json:"gateway"`
	Identity     string    `json:"identity"`
	Subject      string    `json:"subject"`
	Device       string    `json:"device"`
	Role         string    `json:"role"`
	ExpiresAt    time.Time `json:"expires_at"`
	RenewWithin  string    `json:"renew_within"`
	RefreshToken string    `json:"refresh_token"`
}

// StatePath returns the state file within a configuration directory.
func StatePath(dir string) string { return filepath.Join(dir, StateFile) }

// LoadState reads the login record. A missing file reports false rather than an
// error: not being signed in is an ordinary state, not a fault.
func LoadState(path string) (*State, bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, false, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, true, nil
}

// SaveState writes the login record.
func SaveState(path string, s *State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode login state: %w", err)
	}
	return WriteFileAtomic(path, append(raw, '\n'))
}

// DeleteState removes the login record. Removing an absent one is not an error.
func DeleteState(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// RenewalDue reports whether the key is close enough to expiry to renew.
//
// Renewing ahead of expiry rather than at it is what keeps renewal invisible.
// The value a running session read at startup stays valid while the file is
// rewritten under it, so the new key applies at the next launch and no session
// ever sees an expired one. Renewing at expiry would mean the first session
// after it started with a key that no longer worked.
func (s *State) RenewalDue(now time.Time) bool {
	if s.ExpiresAt.IsZero() {
		return true
	}
	window, err := time.ParseDuration(s.RenewWithin)
	if err != nil || window <= 0 {
		// An unreadable window is not a reason to stop renewing; it is a reason
		// to fall back to something sane.
		window = 168 * time.Hour
	}
	return !now.Before(s.ExpiresAt.Add(-window))
}
