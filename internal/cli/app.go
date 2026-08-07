package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"matrix-cli/internal/auth"
	"matrix-cli/internal/config"
	"matrix-cli/internal/matrix"
	"matrix-cli/internal/session"
	"matrix-cli/internal/tui"
)

type MatrixFactory func(config.Config, auth.Credentials) (matrix.API, error)

type App struct {
	Config      config.Store
	Session     session.Store
	HTTP        *http.Client
	NewMatrix   MatrixFactory
	Now         func() time.Time
	OpenBrowser func(string) error

	// Account hooks are set by NewDefault. Leaving them nil preserves the
	// simple single-account setup used by embedders and tests.
	SelectAccount     func(string) (config.Store, session.Store, error)
	ListAccounts      func() ([]string, error)
	SetDefaultAccount func(string) error
	DeleteCrypto      func(string) error
	Account           string
	DefaultAccount    string
}

func NewDefault() (*App, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	selectAccount := func(name string) (config.Store, session.Store, error) {
		accountPath, err := config.AccountPath(path, name)
		if err != nil {
			return nil, nil, err
		}
		return config.FileStore{Path: accountPath}, session.FallbackStore{
			Primary:  session.NewKeyringStoreForAccount(name),
			Fallback: session.FileStore{Path: filepath.Join(filepath.Dir(accountPath), "session.json")},
		}, nil
	}
	preferencesStore := config.AccountPreferencesStore{Path: config.AccountPreferencesPath(path)}
	preferences, err := preferencesStore.Load()
	if err != nil {
		return nil, err
	}
	defaultAccount := preferences.DefaultAccount
	cfgStore, sessionStore, _ := selectAccount(defaultAccount)
	app := &App{
		Config:        cfgStore,
		Session:       sessionStore,
		HTTP:          auth.NewHTTPClient(),
		SelectAccount: selectAccount,
		ListAccounts:  func() ([]string, error) { return config.ListAccounts(path) },
		SetDefaultAccount: func(name string) error {
			if err := preferencesStore.Save(config.AccountPreferences{DefaultAccount: name}); err != nil {
				return err
			}
			return nil
		},
		DeleteCrypto: func(name string) error {
			accountPath, err := config.AccountPath(path, name)
			if err != nil {
				return err
			}
			cryptoPath := filepath.Join(filepath.Dir(accountPath), "crypto.db")
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Remove(cryptoPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return nil
		},
		Account:        defaultAccount,
		DefaultAccount: defaultAccount,
	}
	app.NewMatrix = func(cfg config.Config, creds auth.Credentials) (matrix.API, error) {
		accountPath, err := config.AccountPath(path, app.Account)
		if err != nil {
			return nil, err
		}
		return matrix.NewEncrypted(cfg.HomeserverURL, creds.UserID, creds.DeviceID, creds.AccessToken,
			filepath.Join(filepath.Dir(accountPath), "crypto.db"), []byte(creds.CryptoPickleKey))
	}
	return app, nil
}

func (a *App) Root() *cobra.Command {
	account := a.DefaultAccount
	if account == "" {
		account = "default"
	}
	root := &cobra.Command{
		Use:           "matrix",
		Short:         "A small command-line Matrix client",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return a.useAccount(account)
		},
	}
	root.PersistentFlags().StringVar(&account, "ac", account, "account to use")
	root.PersistentFlags().StringVar(&account, "account", account, "account to use (same as --ac)")
	root.AddCommand(a.loginCommand(), a.logoutCommand(), a.roomsCommand(), a.spacesCommand(), a.sendCommand(), a.watchCommand(), a.chatCommand(), a.tuiCommand(), a.keysCommand(), a.configCommand(), a.accountsCommand(), a.verifyCommand())
	return root
}

func (a *App) useAccount(name string) error {
	if err := config.ValidateAccountName(name); err != nil {
		return err
	}
	if a.SelectAccount != nil {
		cfg, sess, err := a.SelectAccount(name)
		if err != nil {
			return err
		}
		a.Config, a.Session = cfg, sess
	} else if name != "default" {
		return errors.New("this app does not support multiple accounts")
	}
	a.Account = name
	return nil
}

func (a *App) loginCommand() *cobra.Command {
	return a.newLoginCommand("login", false)
}

func (a *App) newLoginCommand(use string, accountArgument bool) *cobra.Command {
	var homeserver, authURL, serverName, username, password, identityProvider string
	var useSSO bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "Authenticate and store a Matrix session",
		Args: func(cmd *cobra.Command, args []string) error {
			if accountArgument {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.NoArgs(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if accountArgument {
				if err := a.useAccount(args[0]); err != nil {
					return err
				}
			}
			previous, previousErr := a.Config.Load()
			if previousErr == nil {
				if homeserver == "" {
					homeserver = previous.HomeserverURL
				}
				if authURL == "" {
					authURL = previous.AuthURL
				}
				if serverName == "" {
					serverName = previous.ServerName
				}
				if username == "" {
					username = previous.Username
				}
				if identityProvider == "" {
					identityProvider = previous.SSOIDP
				}
				if !cmd.Flags().Changed("sso") && previous.AuthMethod == config.AuthMethodSSO {
					useSSO = true
				}
			}
			if !cmd.Flags().Changed("sso") && previous.AuthMethod == "" {
				if previousCreds, loadErr := a.Session.Load(); loadErr == nil && previousCreds.OAuthTokenEndpoint != "" {
					useSSO = true
				}
			}
			if homeserver == "" {
				return errors.New("--homeserver is required for a new account")
			}
			if authURL == "" {
				authURL = homeserver
			}
			if err := config.ValidateBaseURL("homeserver", homeserver); err != nil {
				return err
			}
			if err := config.ValidateBaseURL("authentication", authURL); err != nil {
				return err
			}
			if identityProvider != "" && !useSSO {
				return errors.New("--idp requires --sso")
			}
			if useSSO && cmd.Flags().Changed("password") {
				return errors.New("--password cannot be used with --sso")
			}
			var creds auth.Credentials
			var err error
			if useSSO {
				authenticator := auth.SSOAuthenticator{
					BaseURL: authURL, HomeserverURL: homeserver, Client: a.HTTP, Now: a.Now,
					DeviceName:  "matrix-cli (" + a.Account + ")",
					OpenBrowser: a.OpenBrowser, Output: cmd.ErrOrStderr(),
				}
				creds, err = authenticator.Login(cmd.Context(), identityProvider)
			} else {
				if username == "" {
					return errors.New("--username is required unless --sso is used")
				}
				if password == "" {
					password = os.Getenv("MATRIX_PASSWORD")
				}
				if password == "" {
					password, err = readPassword(cmd.ErrOrStderr(), os.Stdin)
					if err != nil {
						return err
					}
				}
				authenticator := auth.PasswordAuthenticator{BaseURL: authURL, Client: a.HTTP, Now: a.Now, DeviceName: "matrix-cli (" + a.Account + ")"}
				creds, err = authenticator.Login(cmd.Context(), username, password)
			}
			if err != nil {
				return err
			}
			if username == "" {
				username = creds.UserID
			}
			loginClient, err := matrix.New(homeserver, creds.UserID, creds.DeviceID, creds.AccessToken)
			if err != nil {
				return fmt.Errorf("create Matrix session validator: %w", err)
			}
			if err = loginClient.ValidateSession(cmd.Context()); err != nil {
				return fmt.Errorf("login token is not valid at --homeserver: %w", err)
			}
			creds.CryptoPickleKey, err = newCryptoPickleKey()
			if err != nil {
				return err
			}
			authMethod := config.AuthMethodPassword
			if useSSO {
				authMethod = config.AuthMethodSSO
			}
			cfg := config.Config{HomeserverURL: homeserver, AuthURL: authURL, ServerName: serverName, Username: username, AuthMethod: authMethod}
			if useSSO {
				cfg.SSOIDP = identityProvider
			}
			if previousErr == nil {
				cfg.Keybindings = previous.Keybindings
				cfg.ThreadView = previous.ThreadView
				cfg.Color = previous.Color
				cfg.Theme = previous.Theme
			}
			if err := a.persistLogin(cfg, creds); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", creds.UserID)
			return nil
		},
	}
	cmd.Flags().StringVar(&homeserver, "homeserver", "", "Synapse/client API base URL")
	cmd.Flags().StringVar(&authURL, "auth-url", "", "authentication base URL (defaults to homeserver)")
	cmd.Flags().StringVar(&serverName, "server-name", "", "Matrix server name/ID domain")
	cmd.Flags().StringVarP(&username, "username", "u", "", "Matrix username (not needed with --sso)")
	cmd.Flags().StringVar(&password, "password", "", "password (prefer MATRIX_PASSWORD or the prompt)")
	cmd.Flags().BoolVar(&useSSO, "sso", false, "sign in through the homeserver's browser SSO flow")
	cmd.Flags().StringVar(&identityProvider, "idp", "", "SSO identity provider ID advertised by the homeserver")
	return cmd
}

func (a *App) accountsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "accounts",
		Short: "List and add named accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.ListAccounts == nil {
				if _, err := a.Config.Load(); err == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "default")
				}
				return nil
			}
			names, err := a.ListAccounts()
			if err != nil {
				return fmt.Errorf("list accounts: %w", err)
			}
			for _, name := range names {
				fmt.Fprintln(cmd.OutOrStdout(), name)
			}
			return nil
		},
	}
	cmd.AddCommand(a.newLoginCommand("add <name>", true))
	cmd.AddCommand(&cobra.Command{Use: "default <name>", Short: "Set the account used when --ac is omitted", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := a.setDefaultAccount(args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Default account set to %s\n", args[0])
		return nil
	}})
	return cmd
}

func (a *App) setDefaultAccount(name string) error {
	if err := config.ValidateAccountName(name); err != nil {
		return err
	}
	if a.ListAccounts != nil {
		names, err := a.ListAccounts()
		if err != nil {
			return err
		}
		found := false
		for _, candidate := range names {
			if candidate == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("account %q is not configured", name)
		}
	}
	if a.SetDefaultAccount == nil {
		return errors.New("this app cannot change the default account")
	}
	if err := a.SetDefaultAccount(name); err != nil {
		return err
	}
	a.DefaultAccount = name
	return nil
}

type loginSnapshot struct {
	config         config.Config
	configPresent  bool
	credentials    auth.Credentials
	sessionPresent bool
}

func (a *App) snapshotLogin() loginSnapshot {
	var snapshot loginSnapshot
	if cfg, err := a.Config.Load(); err == nil {
		snapshot.config, snapshot.configPresent = cfg, true
	}
	if credentials, err := a.Session.Load(); err == nil {
		snapshot.credentials, snapshot.sessionPresent = credentials, true
	}
	return snapshot
}

func restoreConfig(store config.Store, snapshot loginSnapshot) error {
	if snapshot.configPresent {
		return store.Save(snapshot.config)
	}
	return store.Delete()
}

func restoreSession(store session.Store, snapshot loginSnapshot) error {
	if snapshot.sessionPresent {
		return store.Save(snapshot.credentials)
	}
	return store.Delete()
}

// persistLogin commits config and credentials as one logical operation. Each
// failed stage restores the state observed before the login, including stores
// that mutate their contents before returning an error.
func (a *App) persistLogin(cfg config.Config, credentials auth.Credentials) error {
	snapshot := a.snapshotLogin()
	if err := a.Session.Save(credentials); err != nil {
		rollbackErr := restoreSession(a.Session, snapshot)
		return errors.Join(fmt.Errorf("store session: %w", err), errorWithContext("restore previous session", rollbackErr))
	}
	if err := a.Config.Save(cfg); err != nil {
		configRollbackErr := restoreConfig(a.Config, snapshot)
		sessionRollbackErr := restoreSession(a.Session, snapshot)
		return errors.Join(
			fmt.Errorf("store config: %w", err),
			errorWithContext("restore previous config", configRollbackErr),
			errorWithContext("restore previous session", sessionRollbackErr),
		)
	}
	if a.DeleteCrypto != nil {
		if err := a.DeleteCrypto(a.Account); err != nil {
			configRollbackErr := restoreConfig(a.Config, snapshot)
			sessionRollbackErr := restoreSession(a.Session, snapshot)
			return errors.Join(
				fmt.Errorf("reset old encryption store: %w", err),
				errorWithContext("restore previous config", configRollbackErr),
				errorWithContext("restore previous session", sessionRollbackErr),
			)
		}
	}
	return nil
}

func errorWithContext(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func newCryptoPickleKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate encryption storage key: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(key), nil
}

func readPassword(prompt io.Writer, input *os.File) (string, error) {
	if !term.IsTerminal(int(input.Fd())) {
		return "", errors.New("no password provided and stdin is not a terminal")
	}
	fmt.Fprint(prompt, "Password: ")
	raw, err := term.ReadPassword(int(input.Fd()))
	fmt.Fprintln(prompt)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(raw) == 0 {
		return "", errors.New("password is empty")
	}
	return string(raw), nil
}

type sessionValidator interface {
	ValidateSession(context.Context) error
}

func mergeRefreshedCredentials(previous, refreshed auth.Credentials) auth.Credentials {
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = previous.RefreshToken
	}
	if refreshed.UserID == "" {
		refreshed.UserID = previous.UserID
	}
	if refreshed.DeviceID == "" {
		refreshed.DeviceID = previous.DeviceID
	}
	// The pickle key is local storage state, not an authentication-server
	// credential, so a refresh response can never replace it.
	refreshed.CryptoPickleKey = previous.CryptoPickleKey
	refreshed.OAuthClientID = previous.OAuthClientID
	refreshed.OAuthTokenEndpoint = previous.OAuthTokenEndpoint
	return refreshed
}

func (a *App) refreshCredentials(ctx context.Context, cfg config.Config, creds auth.Credentials) (auth.Credentials, error) {
	var refreshed auth.Credentials
	var err error
	if creds.OAuthTokenEndpoint != "" {
		refreshed, err = auth.RefreshOAuth(ctx, a.HTTP, a.Now, creds)
	} else {
		refreshed, err = (auth.PasswordAuthenticator{BaseURL: cfg.AuthURL, Client: a.HTTP, Now: a.Now}).Refresh(ctx, creds.RefreshToken)
	}
	if err != nil {
		return auth.Credentials{}, err
	}
	refreshed = mergeRefreshedCredentials(creds, refreshed)
	if err = a.Session.Save(refreshed); err != nil {
		return auth.Credentials{}, fmt.Errorf("store refreshed session: %w", err)
	}
	return refreshed, nil
}

type signInRequiredError struct {
	Account string
	Cause   error
}

func (e signInRequiredError) Error() string {
	return fmt.Sprintf("sign-in required for account %q\nThe homeserver rejected the saved session.\nRun `matrix --ac %s login` to authenticate again.", e.Account, e.Account)
}

func (e signInRequiredError) Unwrap() error { return e.Cause }

func (a *App) loadClient(ctx context.Context) (matrix.API, error) {
	cfg, err := a.Config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config (run `matrix login` first): %w", err)
	}
	creds, err := a.Session.Load()
	if err != nil {
		return nil, fmt.Errorf("load session from OS keyring (run `matrix login` first): %w", err)
	}
	if creds.CryptoPickleKey == "" {
		creds.CryptoPickleKey, err = newCryptoPickleKey()
		if err != nil {
			return nil, err
		}
		if err = a.Session.Save(creds); err != nil {
			return nil, fmt.Errorf("store encryption storage key: %w", err)
		}
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if creds.Expiring(now) {
		creds, err = a.refreshCredentials(ctx, cfg, creds)
		if err != nil {
			return nil, fmt.Errorf("refresh session: %w", err)
		}
	}
	factory := a.NewMatrix
	if factory == nil {
		factory = func(cfg config.Config, creds auth.Credentials) (matrix.API, error) {
			return matrix.New(cfg.HomeserverURL, creds.UserID, creds.DeviceID, creds.AccessToken)
		}
	}
	build := func() (matrix.API, error) { return factory(cfg, creds) }
	client, err := build()
	if err != nil {
		return nil, err
	}
	if validator, ok := client.(sessionValidator); ok {
		err = validator.ValidateSession(ctx)
		if err != nil && errors.Is(err, matrix.ErrInvalidSession) && creds.RefreshToken != "" {
			creds, err = a.refreshCredentials(ctx, cfg, creds)
			if err != nil {
				return nil, fmt.Errorf("session was rejected and refresh failed: %w", err)
			}
			client, err = build()
			if err != nil {
				return nil, err
			}
			validator, ok = client.(sessionValidator)
			if ok {
				err = validator.ValidateSession(ctx)
			}
		}
		if err != nil {
			if errors.Is(err, matrix.ErrInvalidSession) {
				return nil, signInRequiredError{Account: a.Account, Cause: err}
			}
			return nil, err
		}
	}
	if initializer, ok := client.(interface{ Init(context.Context) error }); ok {
		if err := initializer.Init(ctx); err != nil {
			return nil, err
		}
	}
	return client, nil
}

func (a *App) verifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Verify this device with another trusted Matrix client",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.loadClient(cmd.Context())
			if err != nil {
				return err
			}
			verifier, ok := client.(interface {
				Verify(context.Context, io.Reader, io.Writer) error
			})
			if !ok {
				return errors.New("this Matrix client does not support device verification")
			}
			return verifier.Verify(cmd.Context(), os.Stdin, cmd.OutOrStdout())
		},
	}
}

func (a *App) logoutCommand() *cobra.Command {
	return &cobra.Command{Use: "logout", Short: "Revoke and remove the current session", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, loadErr := a.loadClient(cmd.Context())
		var remoteErr error
		if loadErr == nil {
			remoteErr = client.Logout(cmd.Context())
		}
		localErr := a.clearLogin()
		if err := errors.Join(remoteErr, localErr); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Logged out")
		return nil
	}}
}

// clearLogin removes the secret before its config and restores the secret if
// config cleanup fails. This prevents a failed logout from orphaning a live
// keyring credential with no account metadata from which to retry cleanup.
func (a *App) clearLogin() error {
	snapshot := a.snapshotLogin()
	if err := a.Session.Delete(); err != nil {
		return errors.Join(
			fmt.Errorf("delete session: %w", err),
			errorWithContext("restore session after cleanup failure", restoreSession(a.Session, snapshot)),
		)
	}
	if err := a.Config.Delete(); err != nil {
		return errors.Join(
			fmt.Errorf("delete config: %w", err),
			errorWithContext("restore config after cleanup failure", restoreConfig(a.Config, snapshot)),
			errorWithContext("restore session after config cleanup failure", restoreSession(a.Session, snapshot)),
		)
	}
	if a.DeleteCrypto != nil {
		if err := a.DeleteCrypto(a.Account); err != nil {
			return fmt.Errorf("delete encryption store: %w", err)
		}
	}
	return nil
}

func (a *App) roomsCommand() *cobra.Command {
	return &cobra.Command{Use: "rooms", Short: "List joined rooms", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		rooms, err := client.Rooms(cmd.Context())
		if err != nil {
			return err
		}
		sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
		for _, room := range rooms {
			if room.Name == "" {
				fmt.Fprintln(cmd.OutOrStdout(), room.ID)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", room.ID, room.Name)
			}
		}
		return nil
	}}
}

func (a *App) spacesCommand() *cobra.Command {
	return &cobra.Command{Use: "spaces", Short: "List joined spaces and their rooms", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		spaces, err := client.Spaces(cmd.Context())
		if err != nil {
			return err
		}
		sort.Slice(spaces, func(i, j int) bool { return spaces[i].ID < spaces[j].ID })
		for _, space := range spaces {
			label := space.Name
			if label == "" {
				label = space.ID
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", space.ID, label)
			for _, room := range space.Children {
				child := room.Name
				if child == "" {
					child = room.ID
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\n", room.ID, child)
			}
		}
		return nil
	}}
}

func (a *App) configCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Show or change account settings", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "thread_view\t%s\n", cfg.EffectiveThreadView())
		color := cfg.Color
		if color == "" {
			color = "default"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "color\t%s\n", color)
		fmt.Fprintf(cmd.OutOrStdout(), "theme\t%s\n", cfg.EffectiveTheme())
		return nil
	}}
	cmd.AddCommand(&cobra.Command{Use: "set <setting> <value>", Short: "Change an account setting", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		setting, value := strings.ToLower(args[0]), strings.ToLower(args[1])
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		switch setting {
		case "thread_view":
			if value != config.ThreadViewFocused && value != config.ThreadViewSplit {
				return fmt.Errorf("invalid thread view %q (use %q or %q)", value, config.ThreadViewFocused, config.ThreadViewSplit)
			}
			cfg.ThreadView = value
		case "color":
			if value == "default" || value == "none" {
				value = ""
			}
			if !config.ValidAccountColor(value) {
				return fmt.Errorf("invalid account color %q (use #RRGGBB or default)", value)
			}
			cfg.Color = value
		case "theme":
			if value != config.ThemeMinimal && value != config.ThemeBoxed {
				return fmt.Errorf("invalid theme %q (use %q or %q)", value, config.ThemeMinimal, config.ThemeBoxed)
			}
			cfg.Theme = value
		default:
			return fmt.Errorf("unknown config setting %q", setting)
		}
		return a.Config.Save(cfg)
	}})
	return cmd
}

func (a *App) keysCommand() *cobra.Command {
	keys := &cobra.Command{Use: "keys", Short: "Show or change chat keybindings", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		actions := make([]string, 0, len(config.DefaultKeybindings))
		for action := range config.DefaultKeybindings {
			actions = append(actions, action)
		}
		sort.Strings(actions)
		for _, action := range actions {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", action, cfg.Key(action))
		}
		return nil
	}}
	keys.AddCommand(&cobra.Command{Use: "set <action> <key>", Short: "Set a chat keybinding", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if !config.ValidKeyAction(args[0]) {
			return fmt.Errorf("unknown key action %q", args[0])
		}
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		if cfg.Keybindings == nil {
			cfg.Keybindings = make(map[string]string)
		}
		cfg.Keybindings[args[0]] = strings.ToLower(args[1])
		return a.Config.Save(cfg)
	}})
	return keys
}

func (a *App) sendCommand() *cobra.Command {
	return &cobra.Command{Use: "send <room> <message>", Short: "Send a plain-text message", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		return client.Send(cmd.Context(), args[0], strings.Join(args[1:], " "))
	}}
}

func (a *App) watchCommand() *cobra.Command {
	return &cobra.Command{Use: "watch [room]", Short: "Stream new messages", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		room := ""
		if len(args) == 1 {
			room = args[0]
		}
		messages, errs := client.Subscribe(cmd.Context(), room)
		for messages != nil || errs != nil {
			select {
			case msg, ok := <-messages:
				if !ok {
					messages = nil
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", msg.RoomID, msg.Sender, msg.Body)
			case syncErr, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if syncErr != nil {
					return syncErr
				}
			case <-cmd.Context().Done():
				return nil
			}
		}
		return nil
	}}
}

type preparedTUIAccounts struct {
	activeName    string
	activeConfig  config.Config
	activeClient  matrix.API
	options       []tui.AccountOption
	notifications <-chan tui.AccountNotification
}

func compactAccountError(err error) string {
	if err == nil {
		return ""
	}
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

func (a *App) prepareTUIAccounts(ctx context.Context, preferred string) (preparedTUIAccounts, error) {
	if a.ListAccounts == nil || a.SelectAccount == nil {
		cfg, err := a.Config.Load()
		if err != nil {
			return preparedTUIAccounts{}, err
		}
		client, err := a.loadClient(ctx)
		return preparedTUIAccounts{activeName: a.Account, activeConfig: cfg, activeClient: client}, err
	}
	names, err := a.ListAccounts()
	if err != nil {
		return preparedTUIAccounts{}, err
	}
	clients := make(map[string]matrix.API, len(names))
	configs := make(map[string]config.Config, len(names))
	options := make([]tui.AccountOption, 0, len(names))
	for _, name := range names {
		option := tui.AccountOption{Name: name, Default: name == a.DefaultAccount}
		if err = a.useAccount(name); err != nil {
			option.Error = compactAccountError(err)
			options = append(options, option)
			continue
		}
		cfg, loadErr := a.Config.Load()
		if loadErr != nil {
			option.Error = compactAccountError(loadErr)
			options = append(options, option)
			continue
		}
		option.Color = cfg.Color
		if creds, loadErr := a.Session.Load(); loadErr == nil {
			option.UserID = creds.UserID
		}
		client, loadErr := a.loadClient(ctx)
		if loadErr != nil {
			option.Error = compactAccountError(loadErr)
		} else {
			clients[name], configs[name] = client, cfg
		}
		options = append(options, option)
	}
	activeName := preferred
	if clients[activeName] == nil {
		activeName = ""
		for _, option := range options {
			if clients[option.Name] != nil {
				activeName = option.Name
				break
			}
		}
	}
	if activeName == "" {
		return preparedTUIAccounts{}, errors.New("none of the configured accounts has a valid session; run `matrix --ac <name> login`")
	}
	if err = a.useAccount(activeName); err != nil {
		return preparedTUIAccounts{}, err
	}

	notifications := startTUIAccountNotifications(ctx, options, clients, activeName)
	return preparedTUIAccounts{
		activeName: activeName, activeConfig: configs[activeName], activeClient: clients[activeName],
		options: options, notifications: notifications,
	}, nil
}

func startTUIAccountNotifications(ctx context.Context, options []tui.AccountOption, clients map[string]matrix.API, activeName string) <-chan tui.AccountNotification {
	notifications := make(chan tui.AccountNotification, 256)
	for _, option := range options {
		name, accountColor, client := option.Name, option.Color, clients[option.Name]
		if client == nil || name == activeName {
			continue
		}
		// Room metadata for background notifications is loaded in the worker so
		// it doesn't hold up rendering the UI.
		messages, errs := client.Subscribe(ctx, "")
		go func() {
			roomNames := make(map[string]string)
			if rooms, roomsErr := client.Rooms(ctx); roomsErr == nil {
				for _, room := range rooms {
					roomNames[room.ID] = room.Name
				}
			}
			for messages != nil || errs != nil {
				select {
				case message, ok := <-messages:
					if !ok {
						messages = nil
						continue
					}
					notification := tui.AccountNotification{Account: name, Color: accountColor, RoomName: roomNames[message.RoomID], Message: message}
					select {
					case notifications <- notification:
					case <-ctx.Done():
						return
					}
				case syncErr, ok := <-errs:
					if !ok {
						errs = nil
						continue
					}
					if syncErr != nil {
						notification := tui.AccountNotification{Account: name, Color: accountColor, Error: compactAccountError(syncErr)}
						select {
						case notifications <- notification:
						case <-ctx.Done():
							return
						}
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	return notifications
}

func (a *App) tuiCommand() *cobra.Command {
	return &cobra.Command{Use: "tui", Short: "Monitor and browse all local Matrix accounts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		activeName := a.Account
		initialRoom := ""
		var savedNotifications []tui.AccountNotification
		savedUnread := make(map[string]int)
		for {
			switchCtx, cancel := context.WithCancel(cmd.Context())
			type prepareResult struct {
				accounts preparedTUIAccounts
				err      error
			}
			preparedCh := make(chan prepareResult, 1)
			done := make(chan struct{})
			go func() {
				prepared, err := a.prepareTUIAccounts(switchCtx, activeName)
				preparedCh <- prepareResult{accounts: prepared, err: err}
				close(done)
			}()
			if err := tui.RunLoading(switchCtx, "Connecting accounts…", done); err != nil {
				cancel()
				return err
			}
			loaded := <-preparedCh
			prepared, err := loaded.accounts, loaded.err
			if err != nil {
				cancel()
				return err
			}
			result, err := tui.RunWithAccounts(switchCtx, prepared.activeClient, prepared.activeConfig, initialRoom, tui.AccountSwitcher{
				Current: prepared.activeName, Accounts: prepared.options, Notifications: prepared.notifications,
				InitialNotifications: savedNotifications, InitialUnread: savedUnread, SetDefault: a.setDefaultAccount,
			})
			cancel()
			savedNotifications, savedUnread = result.Notifications, result.Unread
			if err != nil || result.Destination.Account == "" {
				return err
			}
			activeName, initialRoom = result.Destination.Account, result.Destination.Room
		}
	}}
}

func (a *App) chatCommand() *cobra.Command {
	return &cobra.Command{Use: "chat <room>", Short: "Open an interactive live chat", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		return tui.RunChat(cmd.Context(), client, args[0], cfg)
	}}
}
