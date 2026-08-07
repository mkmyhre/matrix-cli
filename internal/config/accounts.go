package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AccountPreferences contains settings shared across all local accounts.
type AccountPreferences struct {
	DefaultAccount string `json:"default_account"`
}

type AccountPreferencesStore struct{ Path string }

func AccountPreferencesPath(defaultConfigPath string) string {
	return filepath.Join(filepath.Dir(defaultConfigPath), "accounts.json")
}

func (s AccountPreferencesStore) Load() (AccountPreferences, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return AccountPreferences{DefaultAccount: "default"}, nil
	}
	if err != nil {
		return AccountPreferences{}, err
	}
	var preferences AccountPreferences
	if err = json.Unmarshal(raw, &preferences); err != nil {
		return AccountPreferences{}, fmt.Errorf("decode account preferences: %w", err)
	}
	if preferences.DefaultAccount == "" {
		preferences.DefaultAccount = "default"
	}
	if err = ValidateAccountName(preferences.DefaultAccount); err != nil {
		return AccountPreferences{}, fmt.Errorf("invalid default account: %w", err)
	}
	return preferences, nil
}

func (s AccountPreferencesStore) Save(preferences AccountPreferences) error {
	if err := ValidateAccountName(preferences.DefaultAccount); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(preferences, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create account preferences directory: %w", err)
	}
	if err = os.WriteFile(s.Path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write account preferences: %w", err)
	}
	return os.Chmod(s.Path, 0o600)
}
