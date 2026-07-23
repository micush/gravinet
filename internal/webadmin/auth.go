package webadmin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"

	"gravinet/internal/config"
)

// Authenticator verifies an admin username/password.
type Authenticator interface {
	Authenticate(user, pass string) bool
	// Name describes the backend (for logging).
	Name() string
}

// localAuth checks credentials against PBKDF2-HMAC-SHA256 hashes from config.
type localAuth struct {
	users map[string]localCred
}

type localCred struct {
	salt []byte
	hash []byte
	iter int
}

// NewLocalAuth builds a local authenticator from configured users.
func NewLocalAuth(users []config.AdminUser) *localAuth {
	m := make(map[string]localCred, len(users))
	for _, u := range users {
		salt, err1 := hex.DecodeString(u.Salt)
		hash, err2 := hex.DecodeString(u.Hash)
		if err1 != nil || err2 != nil || u.Name == "" || u.Iterations <= 0 {
			continue
		}
		m[u.Name] = localCred{salt: salt, hash: hash, iter: u.Iterations}
	}
	return &localAuth{users: m}
}

func (a *localAuth) Name() string { return "local" }

func (a *localAuth) Authenticate(user, pass string) bool {
	c, ok := a.users[user]
	if !ok {
		// Spend comparable work on unknown users to blunt timing oracles.
		pbkdf2SHA256([]byte(pass), []byte("decoy-salt-decoy"), 100000, 32)
		return false
	}
	got := pbkdf2SHA256([]byte(pass), c.salt, c.iter, len(c.hash))
	return subtle.ConstantTimeCompare(got, c.hash) == 1
}

// GenerateCredential derives a fresh salt + PBKDF2 hash for a password, for
// writing into config (used by `gravinet genpass`).
func GenerateCredential(name, password string, iterations int) (config.AdminUser, error) {
	if iterations <= 0 {
		iterations = 100000
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return config.AdminUser{}, err
	}
	hash := pbkdf2SHA256([]byte(password), salt, iterations, 32)
	return config.AdminUser{
		Name:       name,
		Salt:       hex.EncodeToString(salt),
		Hash:       hex.EncodeToString(hash),
		Iterations: iterations,
	}, nil
}
