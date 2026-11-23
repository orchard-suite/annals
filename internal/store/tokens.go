package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/orchard-suite/annals/internal/people"
)

// TokenPrefix starts every Annals token, so one is easy to spot in a file
// and to tell apart from any other secret.
const TokenPrefix = "an_"

// formerTokenPrefix starts the tokens issued before the product was
// renamed. They stay valid until an admin revokes them.
const formerTokenPrefix = "lg_"

// issuedPrefix reports whether secret starts the way an issued token does.
func issuedPrefix(secret string) bool {
	return strings.HasPrefix(secret, TokenPrefix) || strings.HasPrefix(secret, formerTokenPrefix)
}

// lastUsedEvery limits how often a token's last use is written.
const lastUsedEvery = time.Minute

var lowerBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Token lets one sender post events for one source and read Annals at
// one level. Only its hash is kept.
type Token struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Source    string    `json:"source"`
	Reads     string    `json:"reads"`
	Created   time.Time `json:"created"`
	CreatedBy string    `json:"created_by,omitempty"`
	LastUsed  time.Time `json:"last_used,omitzero"`
	RevokedAt time.Time `json:"revoked_at,omitzero"`
	hash      string
}

// Level is how much of a person the token's caller may see.
func (t Token) Level() people.Level {
	l, _ := people.ParseTokenLevel(t.Reads)
	return l
}

// Mint makes a token named name for source, reading at reads, and returns
// its secret once.
func (s *Store) Mint(ctx context.Context, name, source, reads, by string) (string, Token, error) {
	if _, err := people.ParseTokenLevel(reads); err != nil {
		return "", Token{}, err
	}
	if reads == "" {
		reads = "reader"
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Token{}, err
	}
	secret := TokenPrefix + lowerBase32.EncodeToString(b)
	tok := Token{ID: ulid.Make().String(), Name: strings.TrimSpace(name), Source: source, Reads: reads,
		Created: time.Now().UTC().Truncate(time.Millisecond), CreatedBy: by, hash: hash(secret)}
	if err := s.writeToken(ctx, tok); err != nil {
		return "", Token{}, err
	}
	s.tokMu.Lock()
	s.tokens[tok.hash] = tok
	s.tokMu.Unlock()
	return secret, tok, nil
}

// writeToken writes the token's current row. The newest version wins.
func (s *Store) writeToken(ctx context.Context, t Token) error {
	var lastUsed, revoked *time.Time
	if !t.LastUsed.IsZero() {
		lastUsed = &t.LastUsed
	}
	if !t.RevokedAt.IsZero() {
		revoked = &t.RevokedAt
	}
	return s.conn.Exec(ctx, `INSERT INTO tokens (id, name, source, reads, hash, created, created_by, last_used, revoked_at, version) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Source, t.Reads, t.hash, t.Created, t.CreatedBy, lastUsed, revoked, uint64(time.Now().UnixNano()))
}

func (s *Store) loadTokens(ctx context.Context) error {
	all, err := s.Tokens(ctx)
	if err != nil {
		return err
	}
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.tokens = map[string]Token{}
	for _, t := range all {
		if t.RevokedAt.IsZero() {
			s.tokens[t.hash] = t
		}
	}
	return nil
}

func (s *Store) liveTokens() int {
	s.tokMu.RLock()
	defer s.tokMu.RUnlock()
	return len(s.tokens)
}

// Verify finds the live token a secret belongs to.
func (s *Store) Verify(ctx context.Context, secret string) (Token, bool) {
	if !issuedPrefix(secret) {
		return Token{}, false
	}
	h := hash(secret)
	s.tokMu.RLock()
	tok, ok := s.tokens[h]
	s.tokMu.RUnlock()
	if !ok {
		return Token{}, false
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if now.Sub(tok.LastUsed) > lastUsedEvery {
		tok.LastUsed = now
		s.tokMu.Lock()
		if _, live := s.tokens[h]; live {
			s.tokens[h] = tok
		}
		s.tokMu.Unlock()
		// A missed last-use write never refuses the caller.
		_ = s.writeToken(ctx, tok)
	}
	return tok, true
}

// Revoke ends a token and returns it as it now stands. The row stays so the
// Tokens page can say it was.
func (s *Store) Revoke(ctx context.Context, id string) (Token, error) {
	s.tokMu.Lock()
	var tok Token
	found := false
	for h, t := range s.tokens {
		if t.ID == id {
			tok, found = t, true
			delete(s.tokens, h)
			break
		}
	}
	s.tokMu.Unlock()
	if !found {
		return Token{}, ErrNotFound
	}
	tok.RevokedAt = time.Now().UTC().Truncate(time.Millisecond)
	return tok, s.writeToken(ctx, tok)
}

// Tokens lists every token, live first, newest first.
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.conn.Query(ctx, `SELECT id, name, source, reads, hash, created, created_by, last_used, revoked_at
		FROM tokens FINAL ORDER BY isNotNull(revoked_at), created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var lastUsed, revoked *time.Time
		if err := rows.Scan(&t.ID, &t.Name, &t.Source, &t.Reads, &t.hash, &t.Created, &t.CreatedBy, &lastUsed, &revoked); err != nil {
			return nil, err
		}
		if lastUsed != nil {
			t.LastUsed = lastUsed.UTC()
		}
		if revoked != nil {
			t.RevokedAt = revoked.UTC()
		}
		t.Created = t.Created.UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

func hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
