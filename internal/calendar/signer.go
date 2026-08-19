package calendar

import (
	"time"

	"github.com/vaishnavghenge/vartalaap-server/internal/auth"
)

// JWTSigner is the production TokenSigner, backed by internal/auth. It exists
// because auth exposes package-level functions and Service takes an interface
// so tests can substitute a trivial signer.
type JWTSigner struct{}

func (JWTSigner) SignPurposeToken(userID, purpose, secret string, ttl time.Duration) (string, error) {
	return auth.SignPurposeToken(userID, purpose, secret, ttl)
}

func (JWTSigner) VerifyPurposeToken(tokenStr, purpose, secret string) (string, error) {
	return auth.VerifyPurposeToken(tokenStr, purpose, secret)
}

var _ TokenSigner = JWTSigner{}
