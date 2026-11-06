package pg

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// minIterations refuses servers that ask for a cheap key derivation, which
// would make a captured proof easier to brute-force. PostgreSQL uses 4096.
const minIterations = 4096

// scram runs the client side of SCRAM-SHA-256 (RFC 5802, RFC 7677)
// without channel binding. Passwords are used as given, without SASLprep
// normalization; ASCII passwords are unaffected by that.
type scram struct {
	password        string
	clientNonce     string
	clientFirstBare string
	salted          []byte
	authMessage     string
}

func newSCRAM(user, password, nonce string) *scram {
	user = strings.NewReplacer("=", "=3D", ",", "=2C").Replace(user)
	return &scram{password: password, clientNonce: nonce, clientFirstBare: "n=" + user + ",r=" + nonce}
}

// clientFirst is the first message: no channel binding ("n,,").
func (s *scram) clientFirst() string {
	return "n,," + s.clientFirstBare
}

// clientFinal answers the server's challenge with a proof of the password.
func (s *scram) clientFinal(serverFirst string) (string, error) {
	attrs := map[string]string{}
	for _, part := range strings.Split(serverFirst, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || len(k) != 1 {
			return "", errors.New("pg: malformed SCRAM challenge")
		}
		attrs[k] = v
	}
	if _, ok := attrs["m"]; ok {
		return "", errors.New("pg: SCRAM challenge has an unsupported mandatory extension")
	}
	nonce := attrs["r"]
	if len(nonce) <= len(s.clientNonce) || !strings.HasPrefix(nonce, s.clientNonce) {
		return "", errors.New("pg: SCRAM server nonce does not extend ours")
	}
	salt, err := base64.StdEncoding.DecodeString(attrs["s"])
	if err != nil || len(salt) == 0 {
		return "", errors.New("pg: malformed SCRAM salt")
	}
	iter, err := strconv.Atoi(attrs["i"])
	if err != nil || iter < minIterations {
		return "", fmt.Errorf("pg: SCRAM iteration count %q is below %d", attrs["i"], minIterations)
	}
	s.salted, err = pbkdf2.Key(sha256.New, s.password, salt, iter, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(s.salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + nonce // biws is base64("n,,")
	s.authMessage = s.clientFirstBare + "," + serverFirst + "," + withoutProof
	proof := hmacSHA256(storedKey[:], s.authMessage)
	for i := range proof {
		proof[i] ^= clientKey[i]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

// verifyServer checks that the server, too, knows the password, so a
// server that merely accepts anything cannot pose as the real one.
func (s *scram) verifyServer(serverFinal string) error {
	if e, ok := strings.CutPrefix(serverFinal, "e="); ok {
		return fmt.Errorf("pg: SCRAM failed: %s", e)
	}
	v, ok := strings.CutPrefix(serverFinal, "v=")
	if !ok || s.salted == nil {
		return errors.New("pg: malformed SCRAM final message")
	}
	got, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return errors.New("pg: malformed SCRAM server signature")
	}
	want := hmacSHA256(hmacSHA256(s.salted, "Server Key"), s.authMessage)
	if !hmac.Equal(got, want) {
		return errors.New("pg: SCRAM server signature does not match; the server does not know the password")
	}
	return nil
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
