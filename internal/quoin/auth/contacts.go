package auth

// Contact targets (docs/authentication-design.md §1): only the administrator
// assigns receive targets; operators can never replace them during
// initialization or login. At most one contact per channel
// (user_contacts UNIQUE(user_id, channel)). Replacing a target clears its
// verification and bumps its version, which invalidates every challenge bound
// to the previous target version.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Contact channels (user_contacts.channel CHECK).
const (
	ChannelEmail = "email"
	ChannelSMS   = "sms"
)

// ContactInput is one admin-assigned contact target.
type ContactInput struct {
	Channel string `json:"channel"`
	Target  string `json:"target"`
}

type contactRow struct {
	ID         int64
	UserID     int64
	Channel    string
	Enabled    bool
	Target     string
	Version    int64
	VerifiedAt sql.NullString
}

// validateContactInput enforces bounded, structured targets: an email with a
// single @ and no whitespace, or an E.164-style phone number. Template
// rendering stays structural (no unescaped concatenation, §8).
func validateContactInput(channel, target string) error {
	target = strings.TrimSpace(target)
	switch channel {
	case ChannelEmail:
		if len(target) < 3 || len(target) > 320 || strings.Count(target, "@") != 1 {
			return fmt.Errorf("email must contain exactly one @ and 3 to 320 characters")
		}
		local, domain, _ := strings.Cut(target, "@")
		if local == "" || domain == "" || strings.ContainsAny(target, " \t\r\n") {
			return fmt.Errorf("email must contain a local part and a domain")
		}
	case ChannelSMS:
		trimmed := strings.TrimPrefix(target, "+")
		if len(trimmed) < 5 || len(trimmed) > 20 {
			return fmt.Errorf("phone number must contain 5 to 20 digits")
		}
		for _, r := range trimmed {
			if r < '0' || r > '9' {
				return fmt.Errorf("phone number must contain only digits with an optional leading +")
			}
		}
	default:
		return fmt.Errorf("channel must be email or sms")
	}
	return nil
}

// maskTarget is the only contact shape that ever leaves the server.
func maskTarget(channel, target string) string {
	switch channel {
	case ChannelEmail:
		local, domain, _ := strings.Cut(target, "@")
		prefix := "***"
		if len(local) > 0 {
			prefix = local[:1] + "***"
		}
		return prefix + "@" + domain
	case ChannelSMS:
		digits := strings.TrimPrefix(target, "+")
		if len(digits) > 4 {
			digits = digits[len(digits)-4:]
		}
		return "****" + digits
	default:
		return "***"
	}
}

const contactColumns = `id,user_id,channel,enabled,target,version,verified_at`

func scanContact(row *sql.Row) (contactRow, error) {
	var contact contactRow
	var enabled int
	if err := row.Scan(&contact.ID, &contact.UserID, &contact.Channel, &enabled, &contact.Target, &contact.Version, &contact.VerifiedAt); err != nil {
		return contactRow{}, err
	}
	contact.Enabled = enabled == 1
	return contact, nil
}

func findContactByID(ctx context.Context, reader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (contactRow, error) {
	return scanContact(reader.QueryRowContext(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE id=?`, id))
}

// listMaskedContacts returns the masked projection of a user's contacts.
func listMaskedContacts(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, userID int64) ([]MaskedContact, error) {
	rows, err := reader.QueryContext(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE user_id=? AND enabled=1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts := []MaskedContact{}
	for rows.Next() {
		var contact contactRow
		var enabled int
		if err := rows.Scan(&contact.ID, &contact.UserID, &contact.Channel, &enabled, &contact.Target, &contact.Version, &contact.VerifiedAt); err != nil {
			return nil, err
		}
		contact.Enabled = enabled == 1
		contacts = append(contacts, MaskedContact{
			Locator:      fmt.Sprint(contact.ID),
			Channel:      contact.Channel,
			MaskedTarget: maskTarget(contact.Channel, contact.Target),
			Verified:     contact.VerifiedAt.Valid,
		})
	}
	return contacts, rows.Err()
}

// upsertContact assigns or re-activates one channel inside the caller's
// transaction. A changed target clears verification and bumps the version so
// every outstanding challenge for the old target stops verifying; a retired
// channel coming back is re-activated (and unverified). Returns the stored
// row and whether content changed.
func upsertContact(ctx context.Context, writer flowWriter, userID int64, input ContactInput, now string) (contactRow, bool, error) {
	existing, err := scanContact(writer.QueryRowContext(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE user_id=? AND channel=?`, userID, input.Channel))
	switch {
	case err == nil:
		if existing.Enabled && existing.Target == input.Target {
			return existing, false, nil
		}
		if existing.Target != input.Target {
			if _, err := writer.ExecContext(ctx, `UPDATE user_contacts SET target=?,enabled=1,version=version+1,verified_at=NULL,updated_at=? WHERE id=?`, input.Target, now, existing.ID); err != nil {
				return contactRow{}, false, err
			}
		} else {
			if _, err := writer.ExecContext(ctx, `UPDATE user_contacts SET enabled=1,version=version+1,verified_at=NULL,updated_at=? WHERE id=?`, now, existing.ID); err != nil {
				return contactRow{}, false, err
			}
		}
		updated, err := findContactByID(ctx, writer, existing.ID)
		return updated, true, err
	case errors.Is(err, sql.ErrNoRows):
		result, err := writer.ExecContext(ctx, `INSERT INTO user_contacts(user_id,channel,target,enabled,version,created_at,updated_at) VALUES(?,?,?,1,1,?,?)`, userID, input.Channel, input.Target, now, now)
		if err != nil {
			return contactRow{}, false, err
		}
		id, err := result.LastInsertId()
		if err != nil {
			return contactRow{}, false, err
		}
		created, err := findContactByID(ctx, writer, id)
		return created, true, err
	default:
		return contactRow{}, false, err
	}
}

// retireMissingContacts implements the full-set semantics of SetUserContacts:
// every active channel absent from the desired set is retired — enabled=0,
// unverified, version bumped — never deleted, so challenge and audit foreign
// keys keep their history while the channel stops being usable immediately.
func retireMissingContacts(ctx context.Context, writer flowWriter, userID int64, activeChannels map[string]bool, now string) (bool, error) {
	existing, err := listContactRows(ctx, writer, userID)
	if err != nil {
		return false, err
	}
	changed := false
	for _, contact := range existing {
		if !contact.Enabled || activeChannels[contact.Channel] {
			continue
		}
		if _, err := writer.ExecContext(ctx, `UPDATE user_contacts SET enabled=0,version=version+1,verified_at=NULL,updated_at=? WHERE id=?`, now, contact.ID); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

func listContactRows(ctx context.Context, reader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, userID int64) ([]contactRow, error) {
	rows, err := reader.QueryContext(ctx, `SELECT `+contactColumns+` FROM user_contacts WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	contacts := []contactRow{}
	for rows.Next() {
		var contact contactRow
		var enabled int
		if err := rows.Scan(&contact.ID, &contact.UserID, &contact.Channel, &enabled, &contact.Target, &contact.Version, &contact.VerifiedAt); err != nil {
			return nil, err
		}
		contact.Enabled = enabled == 1
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}
