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
	SelectAccount func(string) (config.Store, session.Store, error)
	ListAccounts  func() ([]string, error)
	DeleteCrypto  func(string) error
	Account       string
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
	cfgStore, sessionStore, _ := selectAccount("default")
	app := &App{
		Config:        cfgStore,
		Session:       sessionStore,
		SelectAccount: selectAccount,
		ListAccounts:  func() ([]string, error) { return config.ListAccounts(path) },
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
		Account: "default",
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
	account := "default"
	root := &cobra.Command{
		Use:           "matrix",
		Short:         "A small command-line Matrix client",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return a.useAccount(account)
		},
	}
	root.PersistentFlags().StringVar(&account, "ac", "default", "account to use")
	root.PersistentFlags().StringVar(&account, "account", "default", "account to use (same as --ac)")
	root.AddCommand(a.loginCommand(), a.logoutCommand(), a.roomsCommand(), a.spacesCommand(), a.sendCommand(), a.watchCommand(), a.chatCommand(), a.tuiCommand(), a.keysCommand(), a.accountsCommand(), a.verifyCommand())
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
			if homeserver == "" {
				return errors.New("--homeserver is required")
			}
			if authURL == "" {
				authURL = homeserver
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
			if a.DeleteCrypto != nil {
				if err := a.DeleteCrypto(a.Account); err != nil {
					return fmt.Errorf("reset old encryption store: %w", err)
				}
			}
			cfg := config.Config{HomeserverURL: homeserver, AuthURL: authURL, ServerName: serverName, Username: username}
			if previous, loadErr := a.Config.Load(); loadErr == nil {
				cfg.Keybindings = previous.Keybindings
			}
			if err := a.Session.Save(creds); err != nil {
				return fmt.Errorf("store session in OS keyring: %w", err)
			}
			if err := a.Config.Save(cfg); err != nil {
				_ = a.Session.Delete()
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
	return cmd
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
				return nil, fmt.Errorf("%w; run `matrix login` again", err)
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
		secretErr := a.Session.Delete()
		configErr := a.Config.Delete()
		var cryptoErr error
		if a.DeleteCrypto != nil {
			cryptoErr = a.DeleteCrypto(a.Account)
		}
		if err := errors.Join(remoteErr, secretErr, configErr, cryptoErr); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Logged out")
		return nil
	}}
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

func (a *App) tuiCommand() *cobra.Command {
	return &cobra.Command{Use: "tui", Short: "Browse spaces and rooms interactively", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := a.Config.Load()
		if err != nil {
			return err
		}
		client, err := a.loadClient(cmd.Context())
		if err != nil {
			return err
		}
		return tui.RunNavigator(cmd.Context(), client, cfg)
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
