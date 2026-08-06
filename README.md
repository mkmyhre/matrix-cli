# matrix-cli

A small Matrix CLI with named accounts, optional end-to-end encryption,
regular subcommands, and live terminal modes.

## Install

```sh
go install -tags goolm ./cmd/matrix
```

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

The authentication URL may differ from the Synapse/client API URL. This supports
Matrix Authentication Service deployments that expose `m.login.password` at the
standard Matrix login path.

```sh
matrix login \
  --homeserver https://matrix.example.com \
  --auth-url https://auth.example.com \
  --server-name example.com \
  --username alice
```

## Multiple accounts

Accounts can point to different homeservers. `default` remains the account used
when no option is supplied, so existing installations continue to work.
Create named accounts either with `accounts add`:

```sh
matrix accounts add work \
  --homeserver https://matrix.work.example \
  --username alice
matrix accounts add personal \
  --homeserver https://matrix.example.org \
  --username alice
```

or by selecting a name while logging in:

```sh
matrix --ac work login --homeserver https://matrix.work.example --username alice
```

List and use them with:

```sh
matrix accounts
matrix --ac default rooms
matrix --ac work tui
matrix --ac personal send '#general:example.org' 'hello'
```

`--account` is also accepted as the long-form alias of `--ac`. Logging out only
removes the selected account (`matrix --ac work logout`). Account names may
contain letters, numbers, `.`, `-`, and `_`.

The password is read without echo. It can also be supplied through
`MATRIX_PASSWORD`. Session tokens are stored in the operating-system keyring.
If no keyring service is available (common in headless Linux development), the
client falls back to `session.json` in its user config directory with mode
`0600`. Non-secret connection metadata is stored separately in `config.json`.

## Commands

```text
matrix login
matrix accounts
matrix accounts add <name>
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
```

## Chat controls

Chat starts in a normal mode inspired by Neovim:

- `j` / `k`: select the next/previous message; `k` at the top loads an older page
- `Ctrl+U`: explicitly load the next older page
- `Enter`: open the selected message's thread in a side-by-side split
- `Esc`: close the current thread; press again to open the room/space navigator
- `i`: enter insert mode
- `Enter`: send from insert mode
- `Esc`: return to normal mode
- `n`: toggle room/space names and Matrix IDs
- `?`: show the effective keybinding help overlay
- `q`: quit from normal mode

Run `matrix tui` to start in the navigator. It shows **Home** first with rooms
that are not in a space, followed by each space and its rooms. Use `j`/`k` and
`Enter` to choose a room; `Esc` returns to the previous room when one is open.

Use `matrix keys` to see effective bindings and, for example,
`matrix keys set toggle_identifiers t` to change one. Bindings are saved in the
normal config file. Room headers use `space -> room -> thread` breadcrumbs, and
thread mode keeps the room timeline visible beside the thread. On narrow
terminals the thread panes stack vertically. The navigator marks unread rooms,
and messages use local timestamps with shortened sender names.
`matrix spaces` prints the joined space hierarchy using names where possible.

`watch` and `chat` use Matrix `/sync` and only display live events after the
initial sync. Chat initially loads 30 recent messages and paginates backward in
30-message pages.

## Encrypted rooms and device verification

After logging in with a `goolm` build, verify the new device against Element or
another already trusted Matrix client:

```sh
matrix --ac default verify
```

Approve the request in the other client, compare the displayed emoji, then type
`y` only when they match. Crypto state is stored per account in a protected
SQLite database. Encrypted text, notice, emote, and thread messages can then be
sent and received. Logging in again or logging out resets that account's local
crypto store because Matrix encryption keys are tied to a specific device.

This initial E2EE support does not yet restore historical keys from server-side
key backup, so older messages whose keys were never shared with this device may
not be available. Browser-based OIDC login is also deferred.

The client validates the saved token with the homeserver before opening the
crypto store. If it reports that the session is no longer valid, log in again.
For local or containerized homeservers, make sure the server database is on a
persistent volume: restarting an ephemeral homeserver invalidates every saved
token and device, and the old local encryption store cannot be reused.
