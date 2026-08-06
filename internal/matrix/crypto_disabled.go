//go:build !goolm

package matrix

// NewEncrypted falls back to the original unencrypted client in regular
// builds. Build with -tags goolm to include pure-Go end-to-end encryption.
func NewEncrypted(homeserverURL, userID, deviceID, accessToken, _ string, _ []byte) (*Client, error) {
	return New(homeserverURL, userID, deviceID, accessToken)
}
