package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// maxUserStoreRetries bounds the read-verify-write retry loop below. A
// gateway restart storm is the only realistic source of repeated conflicts,
// and that settles within a handful of attempts.
const maxUserStoreRetries = 5

// ErrInvalidCredential covers both an unknown username and a wrong password.
// The two must answer identically: distinguishing them would let a caller
// probe which usernames exist (4.6).
var ErrInvalidCredential = errors.New("gateway: invalid credential")

// ErrUserNotFound is used internally where the caller already knows the
// username is meant to exist (e.g. changing a password), so there is no
// enumeration risk in saying so.
var ErrUserNotFound = errors.New("gateway: user not found")

// UserRecord is the verification material the gateway keeps for one user. It
// never carries a recoverable form of the credential (4.2's requirement that
// the gateway authenticate without an external identity provider still
// leaves the credential itself something to protect).
type UserRecord struct {
	PasswordHash string    `json:"passwordHash"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// UserStore holds the gateway's own users and the one-way material used to
// verify their credentials, in exactly one Secret: an external data store
// here would be a second thing that can drift from this one (4.2's "共有
// ゲートウェイは...自身が保持する利用者による認証を提供する").
type UserStore struct {
	Dynamic    dynamic.Interface
	Namespace  string
	SecretName string
	// DataKey is the Secret key holding the JSON-encoded user map. Zero
	// means "users".
	DataKey string
	// BcryptCost overrides bcrypt.DefaultCost. Zero means the default.
	BcryptCost int
}

func (s *UserStore) dataKey() string {
	if s.DataKey != "" {
		return s.DataKey
	}
	return "users"
}

func (s *UserStore) client() dynamic.ResourceInterface {
	return s.Dynamic.Resource(secretGVR).Namespace(s.Namespace)
}

// Lookup returns the record for username, or ok=false if none exists.
func (s *UserStore) Lookup(ctx context.Context, username string) (UserRecord, bool, error) {
	users, _, err := s.read(ctx)
	if err != nil {
		return UserRecord{}, false, err
	}
	rec, ok := users[username]
	return rec, ok, nil
}

// Verify checks password for username against its stored hash.
func (s *UserStore) Verify(ctx context.Context, username, password string) (UserRecord, error) {
	rec, ok, err := s.Lookup(ctx, username)
	if err != nil {
		return UserRecord{}, err
	}
	if !ok {
		return UserRecord{}, ErrInvalidCredential
	}
	if bcrypt.CompareHashAndPassword([]byte(rec.PasswordHash), []byte(password)) != nil {
		return UserRecord{}, ErrInvalidCredential
	}
	return rec, nil
}

// EnsureUser creates username with password if the store has no such user
// yet, and reports whether it already existed. It never overwrites an
// existing user, so calling it again after an operator changed the password
// leaves that change alone (4.3 composed with 4.6).
func (s *UserStore) EnsureUser(ctx context.Context, username, password string) (existed bool, err error) {
	err = s.mutate(ctx, func(users map[string]UserRecord) (bool, error) {
		if _, ok := users[username]; ok {
			existed = true
			return false, nil
		}
		hash, err := s.hash(password)
		if err != nil {
			return false, err
		}
		users[username] = UserRecord{PasswordHash: hash, UpdatedAt: time.Now()}
		return true, nil
	})
	return existed, err
}

// SetPassword replaces username's stored credential material and moves its
// UpdatedAt forward, which is what lets Session Issuer treat every session
// minted before this call as stale (4.6, 4.7).
func (s *UserStore) SetPassword(ctx context.Context, username, password string) error {
	return s.mutate(ctx, func(users map[string]UserRecord) (bool, error) {
		if _, ok := users[username]; !ok {
			return false, ErrUserNotFound
		}
		hash, err := s.hash(password)
		if err != nil {
			return false, err
		}
		users[username] = UserRecord{PasswordHash: hash, UpdatedAt: time.Now()}
		return true, nil
	})
}

func (s *UserStore) hash(password string) (string, error) {
	cost := s.BcryptCost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("gateway: hash credential: %w", err)
	}
	return string(h), nil
}

// read decodes the current user map. obj is nil when the Secret does not
// exist yet, which mutate uses to choose between Create and Update.
func (s *UserStore) read(ctx context.Context) (map[string]UserRecord, *unstructured.Unstructured, error) {
	obj, err := s.client().Get(ctx, s.SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]UserRecord{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("gateway: read secret %s/%s: %w", s.Namespace, s.SecretName, err)
	}
	users := map[string]UserRecord{}
	encoded, found, err := unstructured.NestedString(obj.Object, "data", s.dataKey())
	if err != nil {
		return nil, nil, err
	}
	if found && encoded != "" {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil, fmt.Errorf("gateway: secret %s/%s key %q is not base64: %w", s.Namespace, s.SecretName, s.dataKey(), err)
		}
		if err := json.Unmarshal(raw, &users); err != nil {
			return nil, nil, fmt.Errorf("gateway: secret %s/%s key %q is not valid user data: %w", s.Namespace, s.SecretName, s.dataKey(), err)
		}
	}
	return users, obj, nil
}

// mutate applies fn to the current user map and writes the result back only
// if fn reports a change, retrying from a fresh read on a write conflict.
// The Secret's resourceVersion (carried on obj from read) is what makes the
// write conditional, per the design's read-verify-conditional-write rule.
func (s *UserStore) mutate(ctx context.Context, fn func(users map[string]UserRecord) (changed bool, err error)) error {
	if s.Dynamic == nil || s.Namespace == "" || s.SecretName == "" {
		return errors.New("gateway: incomplete user store configuration")
	}
	var lastErr error
	for attempt := 0; attempt < maxUserStoreRetries; attempt++ {
		users, obj, err := s.read(ctx)
		if err != nil {
			return err
		}
		changed, err := fn(users)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}

		encoded, err := json.Marshal(users)
		if err != nil {
			return fmt.Errorf("gateway: encode users: %w", err)
		}

		if obj == nil {
			secret := newSecretObject(s.Namespace, s.SecretName, map[string][]byte{s.dataKey(): encoded})
			_, err = s.client().Create(ctx, secret, metav1.CreateOptions{})
			if err == nil {
				return nil
			}
			if apierrors.IsAlreadyExists(err) {
				lastErr = err
				continue // another replica created it first; retry from a fresh read
			}
			return fmt.Errorf("gateway: create secret %s/%s: %w", s.Namespace, s.SecretName, err)
		}

		if err := unstructured.SetNestedField(obj.Object, base64.StdEncoding.EncodeToString(encoded), "data", s.dataKey()); err != nil {
			return err
		}
		if _, err := s.client().Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("gateway: update secret %s/%s: %w", s.Namespace, s.SecretName, err)
		}
		return nil
	}
	return fmt.Errorf("gateway: user store update conflicted %d times: %w", maxUserStoreRetries, lastErr)
}
