// Package auth is a lightweight credential store.
// It provides functionality for loading credentials, as well as validating credentials.
package auth

import (
	"encoding/json"
	"io"
	"os"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	// AllUsers is the username that indicates all users, even anonymous users (requests without
	// any BasicAuth information).
	AllUsers = "*"

	// PermAll means all actions permitted.
	PermAll = "all"
	// PermRemove means user is permitted to remove a node.
	PermRemove = "remove"
	// PermExecute means user can access execute endpoint.
	PermExecute = "execute"
	// PermQuery means user can access query endpoint
	PermQuery = "query"
	// PermStatus means user can retrieve node status.
	PermStatus = "status"
	// PermReady means user can retrieve ready status.
	PermReady = "ready"
	// PermBackup means user can backup node.
	PermBackup = "backup"
	// PermLoad means user can load a SQLite dump into a node.
	PermLoad = "load"
)

// BasicAuther is the interface an object must support to return basic auth information.
type BasicAuther interface {
	BasicAuth() (string, string, bool)
}

// HashCache store hash values for users. Safe for use from multiple goroutines.
type HashCache struct {
	mu sync.RWMutex
	m  map[string]map[string]struct{}
}

// NewHashCache returns a instantiated HashCache
func NewHashCache() *HashCache {
	return &HashCache{
		m: make(map[string]map[string]struct{}),
	}
}

// Check returns whether hash is valid for username.
func (h *HashCache) Check(username, hash string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	m, ok := h.m[username]
	if !ok {
		return false
	}

	_, ok = m[hash]
	return ok
}

// Store stores the given hash as a valid hash for username.
func (h *HashCache) Store(username, hash string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.m[username]
	if !ok {
		h.m[username] = make(map[string]struct{})
	}
	h.m[username][hash] = struct{}{}
}

// Credential represents authentication and authorization configuration for a single user.
type Credential struct {
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	Perms    []string `json:"perms,omitempty"`
}

// CredentialsStore stores authentication and authorization information for all users.
type CredentialsStore struct {
	store map[string]string
	perms map[string]map[string]bool

	UseCache  bool
	hashCache *HashCache
}

// NewCredentialsStore returns a new instance of a CredentialStore.
func NewCredentialsStore() *CredentialsStore {
	return &CredentialsStore{
		store:     make(map[string]string),
		perms:     make(map[string]map[string]bool),
		hashCache: NewHashCache(),
		UseCache:  true,
	}
}

// NewCredentialsStoreFromFile returns a new instance of a CredentialStore loaded from a file.
func NewCredentialsStoreFromFile(path string) (*CredentialsStore, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c := NewCredentialsStore()
	return c, c.Load(f)
}

// Load loads credential information from a reader.
func (c *CredentialsStore) Load(r io.Reader) error {
	dec := json.NewDecoder(r)
	// Read open bracket
	_, err := dec.Token()
	if err != nil {
		return err
	}

	var cred Credential
	for dec.More() {
		err := dec.Decode(&cred)
		if err != nil {
			return err
		}
		c.store[cred.Username] = cred.Password
		c.perms[cred.Username] = make(map[string]bool, len(cred.Perms))
		for _, p := range cred.Perms {
			c.perms[cred.Username][p] = true
		}
	}

	// Read closing bracket.
	_, err = dec.Token()
	if err != nil {
		return err
	}

	return nil
}

// Check returns true if the password is correct for the given username.
func (c *CredentialsStore) Check(username, password string) bool {
	pw, ok := c.store[username]
	if !ok {
		return false
	}

	// Simple match with plaintext password in creds?
	if password == pw {
		return true
	}

	// Maybe the given password is a hash -- check if the hash is good
	// for the given user. We use a cache to avoid recomputing a value we
	// previously computed (at substantial compute cost).
	if c.UseCache && c.hashCache.Check(username, password) {
		return true
	}

	// Next, what's in the file may be hashed, so hash the given password
	// and compare.
	if bcrypt.CompareHashAndPassword([]byte(pw), []byte(password)) != nil {
		return false
	}

	// It's good -- cache that result for this user.
	c.hashCache.Store(username, password)
	return true
}

// Password returns the password for the given user.
func (c *CredentialsStore) Password(username string) (string, bool) {
	pw, ok := c.store[username]
	return pw, ok
}

// CheckRequest returns true if b contains a valid username and password.
func (c *CredentialsStore) CheckRequest(b BasicAuther) bool {
	username, password, ok := b.BasicAuth()
	if !ok || !c.Check(username, password) {
		return false
	}
	return true
}

// HasPerm returns true if username has the given perm, either directly or
// via AllUsers. It does not perform any password checking.
func (c *CredentialsStore) HasPerm(username string, perm string) bool {
	if m, ok := c.perms[username]; ok {
		if _, ok := m[perm]; ok {
			return true
		}
	}

	if m, ok := c.perms[AllUsers]; ok {
		if _, ok := m[perm]; ok {
			return true
		}
	}

	return false
}

// HasAnyPerm returns true if username has at least one of the given perms,
// either directly, or via AllUsers. It does not perform any password checking.
func (c *CredentialsStore) HasAnyPerm(username string, perm ...string) bool {
	return func(p []string) bool {
		for i := range p {
			if c.HasPerm(username, p[i]) {
				return true
			}
		}
		return false
	}(perm)
}

// AA authenticates and checks authorization for the given username and password
// for the given perm. If the credential store is nil, then this function always
// returns true. If AllUsers have the given perm, authentication is not done.
// Only then are the credentials checked, and then the perm checked.
func (c *CredentialsStore) AA(username, password, perm string) bool {
	// No credential store? Auth is not even enabled.
	if c == nil {
		return true
	}

	// Is the required perm granted to all users, including anonymous users?
	if c.HasAnyPerm(AllUsers, perm, PermAll) {
		return true
	}

	// At this point a username needs to have been supplied
	if username == "" {
		return false
	}

	// Are the creds good?
	if !c.Check(username, password) {
		return false
	}

	// Is the specified user authorized?
	return c.HasAnyPerm(username, perm, PermAll)
}

// HasPermRequest returns true if the username returned by b has the givem perm.
// It does not perform any password checking, but if there is no username
// in the request, it returns false.
func (c *CredentialsStore) HasPermRequest(b BasicAuther, perm string) bool {
	username, _, ok := b.BasicAuth()
	if !ok {
		return false
	}
	return c.HasPerm(username, perm)
}
