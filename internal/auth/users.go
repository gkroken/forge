package auth

import (
	"fmt"
	"time"

	"forge/internal/meta"
	"golang.org/x/crypto/bcrypt"
)

const nsUsers = "auth:users"

// User is a human operator account.
type User struct {
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName,omitempty"`
	Role        string     `json:"role"` // predefined or custom role name
	CreatedAt   time.Time  `json:"createdAt"`
	LastLogin   *time.Time `json:"lastLogin,omitempty"`
	Disabled    bool       `json:"disabled,omitempty"`
}

type storedUser struct {
	User
	PasswordHash string `json:"passwordHash"`
}

// UserStore manages human user accounts.
type UserStore interface {
	Create(username, password, role string) (User, error)
	List() ([]User, error)
	Get(username string) (User, bool, error)
	Delete(username string) error
	SetRole(username, role string) error
	SetDisabled(username string, disabled bool) error
	// Authenticate checks credentials and updates LastLogin on success.
	// Returns nil, nil when credentials are wrong or the user is disabled.
	Authenticate(username, password string) (*User, error)
}

type userMetaStore struct{ m meta.Store }

// NewUserStore returns a UserStore backed by m.
func NewUserStore(m meta.Store) UserStore { return &userMetaStore{m: m} }

func (s *userMetaStore) Create(username, password, role string) (User, error) {
	if username == "" {
		return User{}, fmt.Errorf("username required")
	}
	var existing storedUser
	if ok, _ := s.m.GetJSON(nsUsers, username, &existing); ok {
		return User{}, fmt.Errorf("user %q already exists", username)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}
	u := User{Username: username, Role: role, CreatedAt: time.Now().UTC()}
	if err := s.m.PutJSON(nsUsers, username, storedUser{User: u, PasswordHash: string(hash)}); err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *userMetaStore) List() ([]User, error) {
	keys, err := s.m.List(nsUsers)
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(keys))
	for _, k := range keys {
		var su storedUser
		if ok, _ := s.m.GetJSON(nsUsers, k, &su); ok {
			out = append(out, su.User)
		}
	}
	return out, nil
}

func (s *userMetaStore) Get(username string) (User, bool, error) {
	var su storedUser
	ok, err := s.m.GetJSON(nsUsers, username, &su)
	return su.User, ok, err
}

func (s *userMetaStore) Delete(username string) error {
	return s.m.Delete(nsUsers, username)
}

func (s *userMetaStore) SetRole(username, role string) error {
	var su storedUser
	ok, err := s.m.GetJSON(nsUsers, username, &su)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q not found", username)
	}
	su.Role = role
	return s.m.PutJSON(nsUsers, username, su)
}

func (s *userMetaStore) SetDisabled(username string, disabled bool) error {
	var su storedUser
	ok, err := s.m.GetJSON(nsUsers, username, &su)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("user %q not found", username)
	}
	su.Disabled = disabled
	return s.m.PutJSON(nsUsers, username, su)
}

func (s *userMetaStore) Authenticate(username, password string) (*User, error) {
	var su storedUser
	ok, err := s.m.GetJSON(nsUsers, username, &su)
	if err != nil {
		return nil, err
	}
	if !ok || su.Disabled {
		return nil, nil
	}
	if err := bcrypt.CompareHashAndPassword([]byte(su.PasswordHash), []byte(password)); err != nil {
		return nil, nil
	}
	now := time.Now().UTC()
	su.LastLogin = &now
	s.m.PutJSON(nsUsers, username, su) //nolint:errcheck
	u := su.User
	u.LastLogin = &now
	return &u, nil
}
