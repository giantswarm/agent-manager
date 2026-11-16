package organizations

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/giantswarm/agent-manager/internal/agents"
	"github.com/giantswarm/agent-manager/internal/identity"
)

// ErrCredentialOperationOtherLogin answers a caller that is not the owner of
// a credential operation (the token's issuer and subject) but has the
// owner's issuer and verified email address: the same person, signed in
// through another connector of the identity provider. It names nothing of
// the operation.
var ErrCredentialOperationOtherLogin = errors.New("this credential operation was started with another login; sign in the same way as when it was started, then open the link again")

// verifiedEmailDigest is the SHA-256 of the caller's email address, compared
// without case, when the identity provider verified it; empty otherwise.
func verifiedEmailDigest(person *identity.Identity) string {
	if person == nil || !person.EmailVerified || person.Email == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.ToLower(person.Email)))
	return hex.EncodeToString(sum[:])
}

// refuseNonOwner is the answer to a caller that does not own operation:
// ErrCredentialOperationOtherLogin for the owner's other login, while the
// operation is neither expired nor cancelled, else not found. A delegated
// caller, another issuer, another or unverified email address, and an
// operation prepared without a verified address all get not found.
func refuseNonOwner(operation *credentialOperation, person *identity.Identity) error {
	digest := verifiedEmailDigest(person)
	if person != nil && !person.Delegated && operation.Issuer == person.Issuer &&
		digest != "" && subtle.ConstantTimeCompare([]byte(operation.EmailDigest), []byte(digest)) == 1 &&
		operation.State != operationCancelled && time.Now().Before(operation.ExpiresAt) {
		return ErrCredentialOperationOtherLogin
	}
	return fmt.Errorf("%w: credential operation", agents.ErrNotFound)
}
