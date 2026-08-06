package session

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
	"matrix-cli/internal/auth"
)

const defaultAccount = "default"

type Store interface {
	Load() (auth.Credentials, error)
	Save(auth.Credentials) error
	Delete() error
}

type KeyringStore struct {
	Service string
	Account string
}

func NewKeyringStore() KeyringStore {
	return NewKeyringStoreForAccount(defaultAccount)
}

func NewKeyringStoreForAccount(account string) KeyringStore {
	return KeyringStore{Service: "matrix-cli", Account: account}
}

func (s KeyringStore) keys() (string, string) {
	service, account := s.Service, s.Account
	if service == "" {
		service = "matrix-cli"
	}
	if account == "" {
		account = defaultAccount
	}
	return service, account
}

func (s KeyringStore) Load() (auth.Credentials, error) {
	service, account := s.keys()
	raw, err := keyring.Get(service, account)
	if err != nil {
		return auth.Credentials{}, err
	}
	var creds auth.Credentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return auth.Credentials{}, fmt.Errorf("decode keyring session: %w", err)
	}
	if !creds.Valid() {
		return auth.Credentials{}, errors.New("invalid session in keyring")
	}
	return creds, nil
}

func (s KeyringStore) Save(creds auth.Credentials) error {
	if !creds.Valid() {
		return errors.New("refusing to save invalid session")
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	service, account := s.keys()
	return keyring.Set(service, account, string(raw))
}

func (s KeyringStore) Delete() error {
	service, account := s.keys()
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
