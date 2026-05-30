package auth

import (
	"time"

	"forge/internal/meta"
)

type metaStore struct{ meta meta.Store }

func (s *metaStore) Create(desc string, grants []Grant, expiresAt *time.Time) (Token, string, error) {
	raw, display := generate()
	hash := hashRaw(raw)
	id := generateID()

	tok := Token{
		ID: id, Description: desc, Grants: grants,
		CreatedAt: time.Now().UTC(), ExpiresAt: expiresAt,
	}
	if err := s.meta.PutJSON(nsTokenByHash, hash, storedToken{Token: tok, SecretHash: hash}); err != nil {
		return Token{}, "", err
	}
	if err := s.meta.PutJSON(nsTokenByID, id, hash); err != nil {
		return Token{}, "", err
	}
	return tok, display, nil
}

func (s *metaStore) Verify(secret string) (*Token, error) {
	hash := hashDisplay(secret)
	if hash == "" {
		return nil, nil // malformed
	}
	var st storedToken
	ok, err := s.meta.GetJSON(nsTokenByHash, hash, &st)
	if err != nil || !ok {
		return nil, err
	}
	if st.ExpiresAt != nil && time.Now().After(*st.ExpiresAt) {
		return nil, nil // expired
	}
	return &st.Token, nil
}

func (s *metaStore) Revoke(id string) error {
	var hash string
	ok, err := s.meta.GetJSON(nsTokenByID, id, &hash)
	if err != nil {
		return err
	}
	if !ok {
		return nil // already gone
	}
	if err := s.meta.Delete(nsTokenByHash, hash); err != nil {
		return err
	}
	return s.meta.Delete(nsTokenByID, id)
}

func (s *metaStore) List() ([]Token, error) {
	keys, err := s.meta.List(nsTokenByHash)
	if err != nil {
		return nil, err
	}
	out := make([]Token, 0, len(keys))
	for _, k := range keys {
		var st storedToken
		if ok, _ := s.meta.GetJSON(nsTokenByHash, k, &st); ok {
			out = append(out, st.Token)
		}
	}
	return out, nil
}

func (s *metaStore) Count() (int, error) {
	keys, err := s.meta.List(nsTokenByHash)
	return len(keys), err
}
