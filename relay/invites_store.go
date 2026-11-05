package relay

import (
	"database/sql"
	"errors"
	"time"
)

// InviteRecord is a passphrase invite: a pairing approved ahead of time. Only
// the passphrase's slow hash is stored.
type InviteRecord struct {
	ID             string    `json:"id"`
	PassphraseHash string    `json:"-"`
	Gateway        string    `json:"gateway,omitempty"`
	NamePrefix     string    `json:"name_prefix,omitempty"`
	MaxUses        int       `json:"max_uses"`
	Uses           int       `json:"uses"`
	ExpiresAt      time.Time `json:"expires_at"`
	ExpectedPubkey string    `json:"expected_pubkey,omitempty"`
	Note           string    `json:"note,omitempty"`
	Revoked        bool      `json:"revoked"`
	CreatedBy      string    `json:"created_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Active reports whether the invite can still be redeemed at now.
func (i *InviteRecord) Active(now time.Time) bool {
	return !i.Revoked && i.Uses < i.MaxUses && now.Before(i.ExpiresAt)
}

const inviteCols = `id, passphrase_hash, gateway, name_prefix, max_uses, uses, expires_at,
	expected_pubkey, note, revoked, created_by, created_at`

func scanInvite(row interface{ Scan(...any) error }) (*InviteRecord, error) {
	var i InviteRecord
	var expires, created int64
	var revoked int
	if err := row.Scan(&i.ID, &i.PassphraseHash, &i.Gateway, &i.NamePrefix, &i.MaxUses, &i.Uses, &expires,
		&i.ExpectedPubkey, &i.Note, &revoked, &i.CreatedBy, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	i.ExpiresAt, i.CreatedAt, i.Revoked = fromMS(expires), fromMS(created), revoked != 0
	return &i, nil
}

// CreateInvite stores a new invite.
func (s *Store) CreateInvite(i InviteRecord) error {
	_, err := s.db.Exec(`INSERT INTO invites (`+inviteCols+`) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, 0, ?, ?)`,
		i.ID, i.PassphraseHash, i.Gateway, i.NamePrefix, i.MaxUses, ms(i.ExpiresAt),
		i.ExpectedPubkey, i.Note, i.CreatedBy, ms(i.CreatedAt))
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// Invites lists invites, newest first.
func (s *Store) Invites() ([]*InviteRecord, error) {
	return s.queryInvites(`SELECT ` + inviteCols + ` FROM invites ORDER BY created_at DESC`)
}

// ActiveInvites lists invites that can still be redeemed at now.
func (s *Store) ActiveInvites(now time.Time) ([]*InviteRecord, error) {
	return s.queryInvites(`SELECT `+inviteCols+` FROM invites
		WHERE revoked = 0 AND uses < max_uses AND expires_at > ? ORDER BY created_at`, ms(now))
}

func (s *Store) queryInvites(q string, args ...any) ([]*InviteRecord, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*InviteRecord
	for rows.Next() {
		i, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite marks an invite unusable. Gateways already created from it are
// unaffected.
func (s *Store) RevokeInvite(id string) error {
	return s.execOne(`UPDATE invites SET revoked = 1 WHERE id = ?`, id)
}

// UseInvite consumes one use, failing with ErrNotFound if none remain (so two
// racing redemptions can't both take the last use).
func (s *Store) UseInvite(id string, now time.Time) error {
	return s.execOne(`UPDATE invites SET uses = uses + 1
		WHERE id = ? AND revoked = 0 AND uses < max_uses AND expires_at > ?`, id, ms(now))
}
