package auth

import (
	"time"

	"forge/internal/meta"
)

type metaStore struct{ meta meta.Store }

func (s *metaStore) Create(desc string, grants []Grant, expiresAt *time.Time, owner ...string) (Token, string, error) {
	if err := ValidateGrants(grants); err != nil {
		return Token{}, "", err
	}
	raw, display := generate()
	hash := hashRaw(raw)
	id := generateID()

	tok := Token{
		ID: id, Description: desc, Grants: grants,
		CreatedAt: time.Now().UTC(), ExpiresAt: expiresAt,
	}
	if len(owner) > 0 {
		tok.Owner = owner[0]
	}
	if err := s.meta.PutJSON(nsTokenByHash, hash, storedToken{Token: tok, SecretHash: hash}); err != nil {
		return Token{}, "", err
	}
	if err := s.meta.PutJSON(nsTokenByID, id, hash); err != nil {
		return Token{}, "", err
	}
	return tok, display, nil
}

// lastUsedResolution is how stale a token's LastUsed may be before Verify
// rewrites it.
const lastUsedResolution = time.Minute

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
	// LastUsed is stamped at a coarse resolution on purpose. Writing it on
	// every request made the token the hottest record in the store — one disk
	// write per authenticated call, on the busiest path there is — for a field
	// nobody reads more precisely than "recently". A minute's resolution keeps
	// the answer useful and drops the writes by orders of magnitude under the
	// parallel fetching npm and mvn do.
	now := time.Now().UTC()
	if st.Token.LastUsed == nil || now.Sub(*st.Token.LastUsed) >= lastUsedResolution {
		st.Token.LastUsed = &now
		_ = s.meta.PutJSON(nsTokenByHash, hash, storedToken{Token: st.Token, SecretHash: hash})
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
