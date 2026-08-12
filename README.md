# matrix-cli

A small Matrix CLI with named accounts, optional end-to-end encryption,
regular subcommands, and live terminal modes.

## Install

Using the Makefile:

```sh
make install
```

Or directly with Go:

```sh
go install -tags goolm ./cmd/matrix
```

Both commands install to `GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is
unset. Run `make build` instead to create `./matrix` in the repository.

The `goolm` build tag enables the pure-Go end-to-end encryption implementation.
A build without that tag still works for unencrypted rooms, but `matrix verify`
and encrypted rooms are unavailable.

This installs `matrix` into `$(go env GOPATH)/bin`. Ensure that directory is on
`PATH`, or run it by its full path.

To build only in the current directory:

```sh
go build -tags goolm -o matrix ./cmd/matrix
```

## Login

### Matrix.org account

To add an existing `@alice:matrix.org` account under the local name `personal`,
use the Matrix.org client endpoint and enter the account password when prompted:

```sh
matrix accounts add personal \
  --homeserver https://matrix-client.matrix.org \
  --server-name matrix.org \
  --username alice
```

The name `personal` is only a local selector; it does not create a new Matrix
account. Use this account on every later command:

```sh
matrix --ac personal rooms
matrix --ac personal tui
matrix --ac personal send '#room:matrix.org' 'hello'
```

Matrix.org currently offers password login, which this example uses. Do not put
the password on the command line. The prompt does not echo it. Accounts that use
browser SSO can use `--sso` as described below.

### Other homeservers

For a normal password-based homeserver:

```sh
matrix login \
  --homeserver https://matrix.example.com \
  --username alice
```

`--auth-url` defaults to `--homeserver`. Set it only when authentication is
served from a different base URL, such as some Matrix Authentication Service
(MAS) deployments:

```sh
matrix login \
  --homeserver https://matrix.example.com \
  --auth-url https://auth.example.com \
  --server-name example.com \
  --username alice
```

The password can alternatively be supplied through `MATRIX_PASSWORD`, which is
useful for non-interactive use but may expose it to the process environment.

### Browser SSO (Keycloak, Okta, and other providers)

Use the standard Matrix SSO flow for an account authenticated by an external
identity provider:

```sh
matrix accounts add work \
  --homeserver https://matrix.work.example \
  --server-name work.example \
  --sso
```

The CLI auto-detects both Matrix `m.login.sso` and delegated Matrix OAuth/MAS.
For legacy SSO it exchanges the one-time Matrix login token. For delegated OAuth
it discovers the issuer from `/.well-known/matrix/client`, dynamically registers
a native client, and uses authorization code flow with PKCE. Both flows start a
temporary callback listener on `127.0.0.1`, print the login URL, and attempt to
open it in the default browser. Tokens returned to the callback are never
printed.

This is not tied to Keycloak: identity-provider configuration belongs to the
homeserver or its delegated authentication service. If a legacy SSO homeserver
advertises multiple providers, it normally shows a chooser. To select one
directly, use the provider ID advertised by the Matrix login endpoint:

```sh
curl -s https://matrix.work.example/_matrix/client/v3/login | jq '.flows[] | select(.type == "m.login.sso")'
matrix accounts add work \
  --homeserver https://matrix.work.example \
  --server-name work.example \
  --sso --idp keycloak
```

`--idp` applies to legacy `m.login.sso`; delegated OAuth services choose their
upstream provider themselves. It is harmless if supplied while delegated OAuth
is auto-detected. `--username` and `--password` are not needed with `--sso`.
The callback is bound only to loopback and protected by a random path and OAuth
state; delegated OAuth also uses PKCE. Login times out after five minutes. On a
remote/headless machine, the printed URL is still provided, but the browser must
be able to reach that machine's loopback callback (for example, through an
appropriate SSH forwarding setup).

## Multiple accounts

Accounts can point to different homeservers. `default` is used when `--ac` is
omitted. Add a named account with:

```sh
matrix accounts add work \
  --homeserver https://matrix.work.example \
  --username alice
```

The equivalent form is:

```sh
matrix --ac work login \
  --homeserver https://matrix.work.example \
  --username alice
```

List and select accounts with:

```sh
matrix accounts
matrix --ac default rooms
matrix --ac work tui
matrix --ac personal send '#general:matrix.org' 'hello'
```

`--account` is the long-form alias for `--ac`. Account names may contain letters,
numbers, `.`, `-`, and `_`. Login, encryption state, appearance, and keybindings
are kept separate for each account. Choose the account used when `--ac` is
omitted with:

```sh
matrix accounts default personal
```

After an account has been configured, reauthenticate without repeating its
homeserver, username, or SSO flags:

```sh
matrix --ac work login
```

The saved authentication method is reused and SSO accounts reopen the browser
flow. Logging out affects only the selected account:

```sh
matrix --ac work logout
```

Session tokens are stored in the operating-system keyring. If no keyring service
is available (common on headless Linux), the client falls back to a
user-readable-only `session.json` (`0600`) in its config directory. Non-secret
connection metadata is stored separately in `config.json`.

## Commands

```text
matrix login
matrix accounts
matrix accounts add <name> [--sso [--idp <provider-id>]]
matrix accounts default <name>
matrix [--ac <name>] logout
matrix [--ac <name>] verify
matrix rooms
matrix spaces
matrix send <room-id-or-alias> <message>
matrix watch [room-id-or-alias]
matrix chat <room-id-or-alias>
matrix tui
matrix keys
matrix keys set <action> <key>
matrix config
matrix config set thread_view <focused|split>
matrix [--ac <name>] config set color <#RRGGBB|default>
matrix [--ac <name>] config set theme <minimal|boxed>
```

## Chat controls

Chat starts in a normal mode inspired by Neovim:

- `j` / `k`: select the next/previous message; `k` at the top loads an older page
- `Ctrl+U`: load up to 10 older messages
- `Enter`: open the selected message's thread in a focused full-width view
- `r`: retry the selected message when sending failed
- `Esc`: close the current thread; press again to open the room/space navigator
- `i`: enter insert mode
- `Enter`: send from insert mode; the message appears immediately with delivery status
- `Esc`: return to normal mode
- `a`: open the local account picker
- `n`: open the cross-account notification inbox
- `v`: toggle room/space names and Matrix IDs
- `?`: show the effective keybinding help overlay
- `q`: quit from normal mode

Run `matrix tui` to start in the navigator. It shows **Home** first with rooms
that are not in a space, followed by each space and its joined rooms. Use `j`/`k`
and `Enter` to choose a room; the list scrolls to keep the selection visible.
`Esc` returns to the previous room when one is open. Unnamed direct messages use
the other member's display name instead of an opaque Matrix room ID.

In the account picker, use `j`/`k` to select an account, `Enter` to switch,
`d` to make it the default for future commands, and `Esc` to close the picker.
Only accounts already added locally are shown. Each healthy account remains
connected while the TUI runs, and its live unread count appears beside it.
Accounts with expired sessions remain visible as **sign-in required** instead of
preventing other accounts from connecting.

Press `n` to open the notification inbox. It keeps the latest 100 incoming
messages in memory, tagged with account, room, sender, timestamp, and preview.
Pressing `Enter` switches account when needed and opens the message's room. The
inbox survives in-TUI account switches but is cleared when the program exits.
The initial implementation tracks new messages received while the TUI is open;
loading historical server unread counts and evaluating full Matrix push rules
are planned follow-ups.

Give accounts distinct header and picker colors to make environments obvious:

```sh
matrix --ac work-dev config set color '#3B82F6'
matrix --ac work-test config set color '#F59E0B'
matrix --ac work-prod config set color '#EF4444'
```

Use `default` instead of a hex value to remove the color. Colors and the default
account are local settings; they are not sent to Matrix.

Use `matrix keys` to see effective bindings and, for example,
`matrix keys set toggle_identifiers t` to change one. Bindings are saved in the
normal config file. The default `minimal` theme uses whitespace, dim separators,
a restrained palette, compact status hints, and `account / space / room / thread`
headers. Normal mode has no badge; `INSERT` appears only while composing. The
selected line uses a subtle left bar. Entering a thread replaces the room
timeline with a full-width **Thread · N replies** view; `Esc` returns to the room.

The previous rounded, bordered appearance remains available per account:

```sh
matrix --ac personal config set theme boxed
```

Use `minimal` to switch back.

To restore the previous room/thread split layout for the selected account:

```sh
matrix --ac personal config set thread_view split
```

Use `focused` to switch back. The navigator marks unread rooms, and messages use
local timestamps with shortened sender names.
`matrix spaces` prints the joined space hierarchy using names where possible.

`watch` and `chat` use Matrix `/sync` and only display live events after the
initial sync. Chat initially loads 10 recent messages; press `Ctrl+U` to load up
to 10 older messages. Encrypted history requests missing room keys from the
account's other verified devices before showing an unavailable placeholder.

## Encrypted rooms and device verification

Encryption support is selected **at build time**. Confirm that the installed
binary was built with the `goolm` tag (see [Install](#install)); an untagged
build cannot read or send encrypted room messages.

Each newly logged-in account is a new Matrix device. After adding an encrypted
account, verify that device using Element or another already trusted client. For
the `personal` example above:

```sh
matrix --ac personal verify
```

1. Keep the command running.
2. Approve the verification request in the trusted client.
3. Compare the emoji shown by both clients.
4. Type `y` only if every emoji matches.

You can then use `matrix --ac personal tui`, `chat`, `watch`, and `send` in
encrypted rooms. Crypto state is stored in a protected SQLite database per
account, separate from every other account.

Important limitations:

- Server-side key backup restore is not implemented. Old encrypted messages may
  remain unreadable unless another device shares their room keys.
- Matrix `m.login.sso` and delegated Matrix OAuth/MAS browser login are
  supported. Generic OIDC issuers must be advertised by a Matrix homeserver;
  arbitrary direct OIDC login is intentionally not supported.
- Logging in again creates a new Matrix device and resets that account's local
  crypto store. Verify the new device again.
- Logging out also removes that account's local crypto store.

The client validates a saved token before opening the crypto store. A rejected
session produces an account-specific sign-in command, for example
`matrix --ac work login`; saved connection and authentication settings are
reused. For local/containerized servers, persist the homeserver database:
recreating it invalidates saved tokens and Matrix device state.
