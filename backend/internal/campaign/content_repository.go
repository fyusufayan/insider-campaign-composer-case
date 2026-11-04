package campaign

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Content versions (campaign_contents) are immutable: this file only ever
// inserts and selects them; there is no UPDATE or DELETE statement for the
// table anywhere in the application. A version row and the campaign's
// current_content_id pointer are written in the same transaction, after the
// campaign row lock, so a failed transaction leaves neither behind.
const (
	contentColumns = `id, version, sms_body, email_subject, email_body, origin, model, prompt_version, created_at`

	sqlSelectContent = `SELECT ` + contentColumns + ` FROM campaign_contents WHERE id = $1 AND campaign_id = $2`

	// Version numbers are assigned under the campaign row lock; the highest
	// stored number must be the current version's (versions only grow and
	// the pointer always moves to the newest one).
	sqlMaxContentVersion = `SELECT COALESCE(max(version), 0) FROM campaign_contents WHERE campaign_id = $1`

	sqlInsertContent = `INSERT INTO campaign_contents
  (campaign_id, version, sms_body, email_subject, email_body, origin, model, prompt_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at`

	// Only the columns a content change may touch are written.
	sqlUpdateCampaignContent = `UPDATE campaigns
SET status = $2, current_content_id = $3, version = $4, updated_at = now()
WHERE id = $1 AND version = $5
RETURNING ` + campaignColumns
)

// SaveContent runs one short transaction: lock the campaign row, read its
// current content version (re-validated), apply mutate, and — if it creates
// a version — insert the immutable row, point current_content_id at it and
// write status and version, then commit. An error from mutate rolls back and
// is returned unchanged; nothing is inserted.
func (r *Repository) SaveContent(ctx context.Context, id ID, mutate ContentMutation) (Campaign, content.Version, error) {
	var (
		outC Campaign
		outV content.Version
	)
	err := r.withLockedCampaign(ctx, id, func(tx pgx.Tx, cur Campaign) error {
		current, err := currentContentTx(ctx, tx, cur)
		if err != nil {
			return err
		}
		next, v, created, err := mutate(cur.clone(), current)
		if err != nil {
			return err
		}
		if !created {
			if current == nil {
				return fmt.Errorf("campaign %s: no content version", id)
			}
			outC, outV = cur, *current
			return nil
		}
		if err := checkContentChange(cur, next, current, v); err != nil {
			return fmt.Errorf("campaign %s: %w", id, err)
		}
		d := v.Content.Draft()
		var model, prompt *string
		if v.Provenance.Model != "" {
			model = &v.Provenance.Model
		}
		if v.Provenance.PromptVersion != "" {
			prompt = &v.Provenance.PromptVersion
		}
		var createdAt time.Time
		if err := tx.QueryRow(ctx, sqlInsertContent, uuidArg(id), v.Number, d.SMSBody, d.EmailSubject, d.EmailBody,
			string(v.Provenance.Origin), model, prompt).Scan(&v.ID, &createdAt); err != nil {
			return fmt.Errorf("campaign %s: insert content version: %w", id, err)
		}
		v.CreatedAt = createdAt.UTC()
		saved, err := scanCampaign(tx.QueryRow(ctx, sqlUpdateCampaignContent,
			uuidArg(id), string(next.Status), v.ID, next.Version, cur.Version))
		if errors.Is(err, pgx.ErrNoRows) { // unreachable while the lock is held
			return &ConflictError{Expected: cur.Version, Current: -1}
		}
		if err != nil {
			return fmt.Errorf("campaign %s: update content pointer: %w", id, err)
		}
		outC, outV = saved, v
		return nil
	})
	if err != nil {
		return Campaign{}, content.Version{}, err
	}
	return outC, outV, nil
}

// checkContentChange asserts that a content mutation changed only what
// sqlUpdateCampaignContent writes, and numbered the version consecutively.
func checkContentChange(cur, next Campaign, current *content.Version, v content.Version) error {
	switch {
	case next.ID != cur.ID:
		return errors.New("content mutation changed the id")
	case next.Version != cur.Version+1:
		return errors.New("content mutation must advance the version by one")
	case !next.Status.Valid():
		return fmt.Errorf("invalid status %q", next.Status)
	case !slices.Equal(next.Channels, cur.Channels) || next.SegmentVersion != cur.SegmentVersion ||
		!ptrEqual(next.ActiveApprovalID, cur.ActiveApprovalID):
		return errors.New("content mutation changed fields other than status and version")
	}
	want := 1
	if current != nil {
		want = current.Number + 1
	}
	if v.Number != want {
		return fmt.Errorf("content version %d does not follow %d", v.Number, want-1)
	}
	return nil
}

func ptrEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// currentContentTx reads the campaign's current content version inside the
// locked transaction and checks that it is the newest one.
func currentContentTx(ctx context.Context, tx pgx.Tx, cur Campaign) (*content.Version, error) {
	var maxVersion int32
	if err := tx.QueryRow(ctx, sqlMaxContentVersion, uuidArg(cur.ID)).Scan(&maxVersion); err != nil {
		return nil, fmt.Errorf("campaign %s: content versions: %w", cur.ID, err)
	}
	if cur.CurrentContentID == nil {
		if maxVersion != 0 {
			return nil, fmt.Errorf("campaign %s: content versions exist but none is current", cur.ID)
		}
		return nil, nil
	}
	v, err := scanContent(tx.QueryRow(ctx, sqlSelectContent, *cur.CurrentContentID, uuidArg(cur.ID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("campaign %s: current content version %d is missing", cur.ID, *cur.CurrentContentID)
	}
	if err != nil {
		return nil, fmt.Errorf("campaign %s: current content version: %w", cur.ID, err)
	}
	if int32(v.Number) != maxVersion {
		return nil, fmt.Errorf("campaign %s: current content version %d is not the newest (%d)", cur.ID, v.Number, maxVersion)
	}
	return &v, nil
}

// GetContent reads one content version of a campaign. The stored text is
// re-validated through content.Parse, never trusted.
func (r *Repository) GetContent(ctx context.Context, id ID, contentID int64) (content.Version, error) {
	v, err := scanContent(r.pool.QueryRow(ctx, sqlSelectContent, contentID, uuidArg(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return content.Version{}, ErrContentNotFound
	}
	if err != nil {
		return content.Version{}, fmt.Errorf("campaign %s: content version %d: %w", id, contentID, err)
	}
	return v, nil
}

// scanContent decodes one campaign_contents row and re-validates it.
func scanContent(row pgx.Row) (content.Version, error) {
	var (
		v             content.Version
		number        int32
		d             content.Draft
		origin        string
		model, prompt *string
		createdAt     time.Time
	)
	if err := row.Scan(&v.ID, &number, &d.SMSBody, &d.EmailSubject, &d.EmailBody, &origin, &model, &prompt, &createdAt); err != nil {
		return content.Version{}, err
	}
	c, err := ParseStoredContent(d)
	if err != nil {
		return content.Version{}, fmt.Errorf("stored content version %d is invalid: %w", v.ID, err)
	}
	v.Number = int(number)
	v.Content = c
	v.Provenance = content.Provenance{Origin: content.Origin(origin)}
	if model != nil {
		v.Provenance.Model = *model
	}
	if prompt != nil {
		v.Provenance.PromptVersion = *prompt
	}
	if err := v.Provenance.Validate(); err != nil {
		return content.Version{}, fmt.Errorf("stored content version %d: %w", v.ID, err)
	}
	if v.Number < 1 {
		return content.Version{}, fmt.Errorf("stored content version %d has number %d", v.ID, v.Number)
	}
	v.CreatedAt = createdAt.UTC()
	return v, nil
}

// ParseStoredContent validates a stored version with content.Parse for the
// channels it has text for (a version is well-formed on its own; whether it
// covers the campaign's current selection is checked where that matters,
// e.g. at prepare). Stored text must already be canonical (NFC, trimmed):
// the application only stores content.Content.Draft().
func ParseStoredContent(d content.Draft) (content.Content, error) {
	var chs []segment.Channel
	if d.SMSBody != nil {
		chs = append(chs, segment.ChannelSMS)
	}
	if d.EmailSubject != nil || d.EmailBody != nil {
		chs = append(chs, segment.ChannelEmail)
	}
	c, res := content.Parse(d, chs)
	if err := res.Err(); err != nil {
		return content.Content{}, err
	}
	if cd := c.Draft(); !strPtrEqual(cd.SMSBody, d.SMSBody) || !strPtrEqual(cd.EmailSubject, d.EmailSubject) ||
		!strPtrEqual(cd.EmailBody, d.EmailBody) {
		return content.Content{}, errors.New("stored content is not in canonical form")
	}
	return c, nil
}

func strPtrEqual(a, b *string) bool { return ptrEqual(a, b) }
