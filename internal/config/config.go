package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix-cli/internal/fileutil"
)

// Config contains non-secret connection metadata. Tokens are stored separately.
const (
	ThreadViewFocused  = "focused"
	ThreadViewSplit    = "split"
	AuthMethodPassword = "password"
	AuthMethodSSO      = "sso"
	ThemeMinimal       = "minimal"
	ThemeBoxed         = "boxed"
)

type Config struct {
	HomeserverURL string            `json:"homeserver_url"`
	AuthURL       string            `json:"auth_url"`
	ServerName    string            `json:"server_name,omitempty"`
	Username      string            `json:"username"`
	AuthMethod    string            `json:"auth_method,omitempty"`
	SSOIDP        string            `json:"sso_idp,omitempty"`
	ThreadView    string            `json:"thread_view,omitempty"`
	Color         string            `json:"color,omitempty"`
	Theme         string            `json:"theme,omitempty"`
	Keybindings   map[string]string `json:"keybindings,omitempty"`
}

var DefaultKeybindings = map[string]string{
	"help":                "?",
	"insert_mode":         "i",
	"load_older":          "ctrl+u",
	"normal_mode":         "esc",
	"move_down":           "j",
	"move_up":             "k",
	"open_thread":         "enter",
	"close_thread":        "esc",
	"switch_account":      "a",
	"set_default_account": "d",
	"notifications":       "n",
	"toggle_identifiers":  "v",
	"quit":                "q",
	"retry_send":          "r",
	"send":                "enter",
}

func (c Config) Key(action string) string {
	if key := c.Keybindings[action]; key != "" {
		return key
	}
	return DefaultKeybindings[action]
}

func ValidKeyAction(action string) bool {
	_, ok := DefaultKeybindings[action]
	return ok
}

func ValidateBaseURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%s URL is invalid: %w", name, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s URL must be an absolute HTTP(S) URL", name)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s URL must not contain credentials", name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s URL must not contain a query or fragment", name)
	}
	return nil
}

func ValidAccountColor(color string) bool {
	if color == "" {
		return true
	}
	if len(color) != 7 || color[0] != '#' {
		return false
	}
	for _, char := range color[1:] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func (c Config) EffectiveTheme() string {
	if c.Theme == ThemeBoxed {
		return ThemeBoxed
	}
	return ThemeMinimal
}

func (c Config) EffectiveThreadView() string {
	if c.ThreadView == ThreadViewSplit {
		return ThreadViewSplit
	}
	return ThreadViewFocused
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.HomeserverURL) == "" {
		return errors.New("homeserver URL is required")
	}
	if err := ValidateBaseURL("homeserver", c.HomeserverURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.AuthURL) == "" {
		return errors.New("authentication URL is required")
	}
	if err := ValidateBaseURL("authentication", c.AuthURL); err != nil {
		return err
	}
	if strings.TrimSpace(c.Username) == "" {
		return errors.New("username is required")
	}
	if c.AuthMethod != "" && c.AuthMethod != AuthMethodPassword && c.AuthMethod != AuthMethodSSO {
		return fmt.Errorf("invalid authentication method %q", c.AuthMethod)
	}
	if c.ThreadView != "" && c.ThreadView != ThreadViewFocused && c.ThreadView != ThreadViewSplit {
		return fmt.Errorf("invalid thread view %q (use %q or %q)", c.ThreadView, ThreadViewFocused, ThreadViewSplit)
	}
	if !ValidAccountColor(c.Color) {
		return fmt.Errorf("invalid account color %q (use #RRGGBB)", c.Color)
	}
	if c.Theme != "" && c.Theme != ThemeMinimal && c.Theme != ThemeBoxed {
		return fmt.Errorf("invalid theme %q (use %q or %q)", c.Theme, ThemeMinimal, ThemeBoxed)
	}
	if err := c.validateKeybindings(); err != nil {
		return err
	}
	return nil
}

func (c Config) validateKeybindings() error {
	for action, key := range c.Keybindings {
		if !ValidKeyAction(action) {
			return fmt.Errorf("unknown keybinding action %q", action)
		}
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("keybinding for %q cannot be empty", action)
		}
		if key != strings.TrimSpace(key) {
			return fmt.Errorf("keybinding for %q contains surrounding whitespace", action)
		}
	}

	actions := make([]string, 0, len(DefaultKeybindings))
	for action := range DefaultKeybindings {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	for i, first := range actions {
		for _, second := range actions[i+1:] {
			if c.Key(first) != c.Key(second) || compatibleKeybindingPair(first, second) {
				continue
			}
			return fmt.Errorf("keybindings for %q and %q conflict on %q", first, second, c.Key(first))
		}
	}
	return nil
}

func compatibleKeybindingPair(first, second string) bool {
	pair := first + ":" + second
	return pair == "close_thread:normal_mode" || pair == "open_thread:send"
}

type Store interface {
	Load() (Config, error)
	Save(Config) error
	Delete() error
}

type FileStore struct{ Path string }

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find config directory: %w", err)
	}
	return filepath.Join(dir, "matrix-cli", "config.json"), nil
}

// ValidateAccountName keeps account names safe to use as directory and keyring
// identifiers while still allowing convenient names such as "work-main".
func ValidateAccountName(name string) error {
	if name == "" {
		return errors.New("account name is required")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid account name %q", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid account name %q (use letters, numbers, '.', '-' or '_')", name)
	}
	return nil
}

// AccountPath returns the config path for an account. The default account uses
// the original path for backwards compatibility with existing installations.
func AccountPath(defaultPath, name string) (string, error) {
	if err := ValidateAccountName(name); err != nil {
		return "", err
	}
	if name == "default" {
		return defaultPath, nil
	}
	return filepath.Join(filepath.Dir(defaultPath), "accounts", name, "config.json"), nil
}

// ListAccounts lists accounts that have a saved config.
func ListAccounts(defaultPath string) ([]string, error) {
	var names []string
	if _, err := os.Stat(defaultPath); err == nil {
		names = append(names, "default")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	entries, err := os.ReadDir(filepath.Join(filepath.Dir(defaultPath), "accounts"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return names, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || ValidateAccountName(entry.Name()) != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(defaultPath), "accounts", entry.Name(), "config.json")); err == nil {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s FileStore) Load() (Config, error) {
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func (s FileStore) Save(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := fileutil.WriteFileAtomic(s.Path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func (s FileStore) Delete() error {
	err := os.Remove(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
