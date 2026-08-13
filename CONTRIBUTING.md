# Contributing

Bug reports and pull requests are welcome.

## Development

The project requires Go 1.24.4 or newer. Clone the repository, then run:

```sh
make check
```

This verifies formatting and modules, runs tests with and without encryption,
and runs `go vet`. Build an encryption-enabled binary with:

```sh
make build
```

## Pull requests

- Keep changes focused and explain the user-facing reason for them.
- Add or update tests when behavior changes.
- Update the README when commands, configuration, or limitations change.
- Run `make check` before submitting.

AI-assisted contributions are accepted. Contributors remain responsible for
reviewing, testing, and licensing everything they submit.

For vulnerabilities, follow [SECURITY.md](SECURITY.md) instead of opening a
public issue.
