//go:build goolm

package matrix

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/goolm"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var registerGoolm sync.Once

type encryptedSupport struct {
	client *Client
	helper *cryptohelper.CryptoHelper
	path   string
}

// NewEncrypted creates a client with a persistent pure-Go Olm/Megolm store.
func NewEncrypted(homeserverURL, userID, deviceID, accessToken, cryptoPath string, pickleKey []byte) (*Client, error) {
	registerGoolm.Do(goolm.Register)
	client, err := New(homeserverURL, userID, deviceID, accessToken)
	if err != nil {
		return nil, err
	}
	if deviceID == "" {
		return nil, errors.New("the login response did not include a device ID; log in again to enable encryption")
	}
	if err := os.MkdirAll(filepath.Dir(cryptoPath), 0o700); err != nil {
		return nil, fmt.Errorf("create crypto store directory: %w", err)
	}
	helper, err := cryptohelper.NewCryptoHelper(client.raw, pickleKey, cryptoPath)
	if err != nil {
		return nil, fmt.Errorf("create crypto helper: %w", err)
	}
	client.crypto = &encryptedSupport{client: client, helper: helper, path: cryptoPath}
	return client, nil
}

func (e *encryptedSupport) Init(ctx context.Context) error {
	if err := e.helper.Init(ctx); err != nil {
		return fmt.Errorf("initialize end-to-end encryption: %w", err)
	}
	e.client.raw.Crypto = e.helper
	_ = os.Chmod(e.path, 0o600)
	return nil
}

func (e *encryptedSupport) Decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	decrypted, err := e.helper.Decrypt(ctx, evt)
	if !errors.Is(err, cryptohelper.NoSessionFound) || evt == nil {
		return decrypted, err
	}

	// Timeline history isn't processed by the sync callback that normally
	// requests missing Megolm sessions. Explicitly request the room key from
	// the sender and our other devices, then briefly wait for /sync to receive
	// it before showing an undecryptable placeholder.
	content := evt.Content.AsEncrypted()
	if content.SessionID == "" || content.SenderKey == "" {
		return nil, err
	}
	e.helper.RequestSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, evt.Sender, content.DeviceID)
	if !e.helper.WaitForSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, 4*time.Second) {
		return nil, err
	}
	return e.helper.Decrypt(ctx, evt)
}

type verificationCallbacks struct {
	in     io.Reader
	out    io.Writer
	helper *verificationhelper.VerificationHelper
	done   chan struct{}
	errs   chan error
}

func (c *verificationCallbacks) fail(err error) {
	select {
	case c.errs <- err:
	default:
	}
}
func (c *verificationCallbacks) VerificationRequested(context.Context, id.VerificationTransactionID, id.UserID, id.DeviceID) {
}
func (c *verificationCallbacks) VerificationReady(ctx context.Context, txnID id.VerificationTransactionID, deviceID id.DeviceID, supportsSAS, _ bool, _ *verificationhelper.QRCode) {
	if !supportsSAS {
		c.fail(errors.New("the other device does not support emoji/SAS verification"))
		return
	}
	fmt.Fprintf(c.out, "Verification accepted by device %s.\n", deviceID)
	// VerificationHelper invokes callbacks while holding its transaction lock.
	// StartSAS acquires that same lock, so it must run after this callback returns.
	go func() {
		if err := c.helper.StartSAS(ctx, txnID); err != nil {
			c.fail(fmt.Errorf("start SAS verification: %w", err))
		}
	}()
}
func (c *verificationCallbacks) ShowSAS(ctx context.Context, txnID id.VerificationTransactionID, emojis []rune, descriptions []string, decimals []int) {
	fmt.Fprintln(c.out, "Compare these emoji with the other Matrix client:")
	for i, emoji := range emojis {
		description := ""
		if i < len(descriptions) {
			description = descriptions[i]
		}
		fmt.Fprintf(c.out, "%c %s", emoji, description)
		if i+1 < len(emojis) {
			fmt.Fprint(c.out, "   ")
		}
	}
	fmt.Fprintln(c.out)
	if len(emojis) == 0 && len(decimals) > 0 {
		fmt.Fprintf(c.out, "Decimals: %v\n", decimals)
	}
	fmt.Fprint(c.out, "Do they match? [y/N] ")
	line, err := bufio.NewReader(c.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		c.fail(fmt.Errorf("read verification confirmation: %w", err))
		return
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		c.fail(errors.New("verification was not confirmed"))
		return
	}
	// As with VerificationReady, ShowSAS is called while the helper's
	// transaction lock is held. Confirm only after returning from the callback.
	go func() {
		if err := c.helper.ConfirmSAS(ctx, txnID); err != nil {
			c.fail(fmt.Errorf("confirm SAS verification: %w", err))
		}
	}()
}
func (c *verificationCallbacks) VerificationCancelled(_ context.Context, _ id.VerificationTransactionID, code event.VerificationCancelCode, reason string) {
	c.fail(fmt.Errorf("verification cancelled (%s): %s", code, reason))
}
func (c *verificationCallbacks) VerificationDone(context.Context, id.VerificationTransactionID, event.VerificationMethod) {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

func (e *encryptedSupport) Verify(ctx context.Context, in io.Reader, out io.Writer) error {
	callbacks := &verificationCallbacks{in: in, out: out, done: make(chan struct{}), errs: make(chan error, 1)}
	helper := verificationhelper.NewVerificationHelper(e.client.raw, e.helper.Machine(), nil, callbacks, false, false, true)
	callbacks.helper = helper
	if err := helper.Init(ctx); err != nil {
		return fmt.Errorf("initialize verification: %w", err)
	}
	e.client.raw.Verification = helper

	syncCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	syncErr := make(chan error, 1)
	go func() { syncErr <- e.client.raw.SyncWithContext(syncCtx) }()

	txnID, err := helper.StartVerification(ctx, e.client.raw.UserID)
	if err != nil {
		return fmt.Errorf("request verification: %w", err)
	}
	fmt.Fprintf(out, "Verification requested for %s (transaction %s). Approve it in a trusted Matrix client.\n", e.client.raw.UserID, txnID)
	select {
	case <-callbacks.done:
		fmt.Fprintln(out, "Device verification complete.")
		return nil
	case err = <-callbacks.errs:
		return err
	case err = <-syncErr:
		if err == nil || errors.Is(err, context.Canceled) {
			return errors.New("sync stopped before verification completed")
		}
		return fmt.Errorf("verification sync: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}
