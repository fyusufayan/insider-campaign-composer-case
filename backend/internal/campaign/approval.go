package campaign

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Campaign-side half of approval preparation (ARCHITECTURE §5.2, §5.5,
// §7.9, §9). The approval package owns the transaction, the approval row and
// the held delivery rows; this file owns the campaign state machine for it
// (pure functions) and the campaign-row statements the approval transaction
// runs after it has locked the row.

// OpPrepareApproval is the prepare operation (§5.2).
const OpPrepareApproval Operation = "prepare_approval"

// reasonApproved explains why prepare refuses an approved campaign.
const reasonApproved = "the campaign is approved; withdraw the approval before preparing a new one"

// CheckPrepareApproval reports whether an approval may be prepared now
// (§5.5): status content_ready or pending_approval (the pending approval is
// superseded by the caller), a stored rule, a stored content brief, a
// current content version and at least one selected channel. It does not
// look at the content text; see ReviewForApproval.
//
// approved is refused (CP14 decision, ARCHITECTURE §9.1): §5.2 lists
// approved → prepare → pending_approval, but the prepare request has no way
// to say "replace the confirmed approval", so a plain prepare (e.g. a stale
// "Review & approve" click) would silently revoke what the marketer
// confirmed. Until an explicit contract exists (withdraw, CP15), the
// marketer must withdraw first. An approved approval that has expired is
// closed by lazy expiry before this check, so the campaign is content_ready
// again and may be prepared.
func (c Campaign) CheckPrepareApproval() error {
	switch c.Status {
	case StatusContentReady:
		if c.ActiveApprovalID != nil { // impossible for a consistent row
			return fmt.Errorf("campaign %s: content_ready with an active approval", c.ID)
		}
	case StatusPendingApproval:
		if c.ActiveApprovalID == nil { // impossible for a consistent row
			return fmt.Errorf("campaign %s: %s without an active approval", c.ID, c.Status)
		}
	case StatusApproved:
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: reasonApproved}
	case StatusDraft:
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: "interpret the brief first"}
	case StatusSegmented:
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: "generate or write content first"}
	default:
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: "the campaign can no longer be approved"}
	}
	if c.Segment.IsZero() || c.ContentBrief == nil { // impossible for a consistent row
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: "the campaign has no interpretation"}
	}
	if c.CurrentContentID == nil { // impossible for a consistent row in these statuses
		return &StateError{Op: OpPrepareApproval, Status: c.Status, Reason: "the campaign has no content"}
	}
	if len(c.Channels) == 0 {
		return &ValidationError{Problems: segment.Errors{{Path: "channels", Code: segment.CodeRequired,
			Message: "select at least one channel before preparing an approval",
			Allowed: []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}}}}
	}
	return nil
}

// CheckPrepareAt is CheckPrepareApproval preceded by the optimistic
// concurrency check: the marketer prepares what they saw at
// expectedVersion, never a newer state.
func (c Campaign) CheckPrepareAt(expectedVersion int) error {
	if c.Version != expectedVersion {
		return &ConflictError{Expected: expectedVersion, Current: c.Version}
	}
	return c.CheckPrepareApproval()
}

// ApprovalReview is the content review computed at prepare: the stored
// version re-validated for the selected channels against the current content
// brief. It contains no customer data (the rendered example uses
// content.ExampleValues).
type ApprovalReview struct {
	// Validation is ValidateStored of the version: no errors; warnings
	// include channel_not_selected, first_name_not_used and every offer
	// policy finding.
	Validation content.Result
	// OfferFindings are the unauthorized_offer / unauthorized_claim warnings
	// of Validation (§7.9: shown to the marketer before confirm). They do not
	// block preparation; the keyword check is conservative and incomplete.
	OfferFindings []segment.FieldError
	// Offer is the authorized offer of the content brief the review used.
	Offer ai.Offer
}

// ReviewForApproval re-validates the current content version v for
// preparation (§7.9 "Slice 3 requirement", §8 "applied identically … and
// prepare"). It is pure.
//
//   - v must be the version the campaign points at.
//   - The stored content brief must parse; otherwise the offer policy could
//     not run and the review would silently omit it.
//   - content.Parse for the selected channels (ValidateStored): a selected
//     channel without text, or any limit violation, is a *ValidationError
//     (422 validation_failed with field errors). Text for a channel that is
//     not selected is a warning only (it is never sent: the approval binds
//     the selected channels).
//   - Offer policy findings are kept as warnings and listed in
//     OfferFindings; they never block here, whatever the version's origin.
func (c Campaign) ReviewForApproval(v content.Version) (ApprovalReview, error) {
	if err := c.checkCurrentContent(&v); err != nil {
		return ApprovalReview{}, err
	}
	brief, problems := ai.ParseContentBrief(c.ContentBrief)
	if len(problems) > 0 {
		return ApprovalReview{}, fmt.Errorf("campaign %s: stored content brief is invalid: %w", c.ID, problems)
	}
	res := c.ValidateStored(v)
	if !res.OK() {
		return ApprovalReview{}, &ValidationError{Problems: res.Errors}
	}
	for _, ch := range c.Channels { // implied by ValidateStored; asserted
		if !v.Content.Has(ch) {
			return ApprovalReview{}, fmt.Errorf("campaign %s: content version %d has no %s text", c.ID, v.Number, ch)
		}
	}
	review := ApprovalReview{Validation: res, Offer: brief.Offer, OfferFindings: []segment.FieldError{}}
	for _, w := range res.Warnings {
		if w.Code == ai.CodeUnauthorizedOffer || w.Code == ai.CodeUnauthorizedClaim {
			review.OfferFindings = append(review.OfferFindings, w)
		}
	}
	return review, nil
}

// WithPreparedApproval applies a successful preparation (§5.2 prepare):
// status → pending_approval, active_approval_id → approvalID, version +1.
// The checks of CheckPrepareAt are repeated, so the transition can only be
// applied to the state that was checked. It is pure.
func (c Campaign) WithPreparedApproval(expectedVersion int, approvalID ID) (Campaign, error) {
	if err := c.CheckPrepareAt(expectedVersion); err != nil {
		return Campaign{}, err
	}
	next := c.clone()
	next.Status = StatusPendingApproval
	next.ActiveApprovalID = &approvalID
	next.Version++
	return next, nil
}

// WithApprovalClosed applies the end of the live approval approvalID by
// expiry or revocation without a new one (§5.2: expiry / withdraw →
// content_ready, active_approval_id cleared). It is pure.
//
// Like every persisted campaign state change (§11.1) it advances the
// version by exactly one, so a request based on the pre-expiry state is a
// version conflict. (A supersede inside prepare needs no separate
// transition: WithPreparedApproval replaces the active approval and
// advances the version once.)
func (c Campaign) WithApprovalClosed(approvalID ID) (Campaign, error) {
	if c.Status != StatusPendingApproval && c.Status != StatusApproved {
		return Campaign{}, fmt.Errorf("campaign %s: cannot close an approval in status %s", c.ID, c.Status)
	}
	if c.ActiveApprovalID == nil || *c.ActiveApprovalID != approvalID {
		return Campaign{}, fmt.Errorf("campaign %s: approval %s is not the active approval", c.ID, approvalID)
	}
	next := c.clone()
	next.ActiveApprovalID = nil
	next.Status = next.baseStatus()
	next.Version++
	return next, nil
}

// ---- Confirmation and withdrawal (CP15, §5.2, §5.5) ----

// Operations of the approval decisions.
const (
	OpConfirmApproval  Operation = "confirm_approval"
	OpWithdrawApproval Operation = "withdraw_approval"
)

// CheckConfirmAt reports whether the approval approvalID may be confirmed
// on top of expectedVersion (§5.2 pending_approval → confirm → approved):
// the version matches (optimistic concurrency) and the campaign is
// pending_approval with approvalID as its active approval. The approval
// row's own state, expiry and binding are checked by the approval
// transaction under the same lock.
func (c Campaign) CheckConfirmAt(expectedVersion int, approvalID ID) error {
	if c.Version != expectedVersion {
		return &ConflictError{Expected: expectedVersion, Current: c.Version}
	}
	active := c.ActiveApprovalID != nil && *c.ActiveApprovalID == approvalID
	switch {
	case c.Status == StatusPendingApproval && active:
		return nil
	case c.Status == StatusApproved && active:
		return &StateError{Op: OpConfirmApproval, Status: c.Status, Reason: "the approval is already confirmed"}
	case c.Status == StatusPendingApproval || c.Status == StatusApproved:
		return &StateError{Op: OpConfirmApproval, Status: c.Status, Reason: "this is not the campaign's current approval; reload the campaign"}
	case c.Status == StatusContentReady || c.Status == StatusSegmented || c.Status == StatusDraft:
		return &StateError{Op: OpConfirmApproval, Status: c.Status, Reason: "no approval is pending; prepare a new one"}
	default:
		return &StateError{Op: OpConfirmApproval, Status: c.Status, Reason: "the campaign can no longer be approved"}
	}
}

// WithConfirmedApproval applies a successful confirmation (§5.2 confirm):
// status → approved, version +1; the active approval is unchanged. The
// checks of CheckConfirmAt are repeated. It is pure.
func (c Campaign) WithConfirmedApproval(expectedVersion int, approvalID ID) (Campaign, error) {
	if err := c.CheckConfirmAt(expectedVersion, approvalID); err != nil {
		return Campaign{}, err
	}
	next := c.clone()
	next.Status = StatusApproved
	next.Version++
	return next, nil
}

// CheckWithdrawAt reports whether the live approval approvalID may be
// withdrawn on top of expectedVersion (§5.2 pending_approval/approved →
// withdraw → content_ready). A consumed approval (sending and finished
// campaigns) is never withdrawn: cancellation of a send is a different
// operation (§10.8). The transition itself is WithApprovalClosed.
func (c Campaign) CheckWithdrawAt(expectedVersion int, approvalID ID) error {
	if c.Version != expectedVersion {
		return &ConflictError{Expected: expectedVersion, Current: c.Version}
	}
	active := c.ActiveApprovalID != nil && *c.ActiveApprovalID == approvalID
	switch {
	case (c.Status == StatusPendingApproval || c.Status == StatusApproved) && active:
		return nil
	case c.Status == StatusPendingApproval || c.Status == StatusApproved:
		return &StateError{Op: OpWithdrawApproval, Status: c.Status, Reason: "this is not the campaign's current approval; reload the campaign"}
	case c.Status == StatusContentReady || c.Status == StatusSegmented || c.Status == StatusDraft:
		return &StateError{Op: OpWithdrawApproval, Status: c.Status, Reason: "the campaign has no live approval to withdraw"}
	default:
		return &StateError{Op: OpWithdrawApproval, Status: c.Status, Reason: "the approval has been used for sending and can no longer be withdrawn"}
	}
}

// ---- Statements run inside the approval transaction ----

const (
	// Only the columns an approval transition may touch are written.
	sqlUpdateCampaignApproval = `UPDATE campaigns
SET status = $2, active_approval_id = $3, version = $4, updated_at = now()
WHERE id = $1 AND version = $5
RETURNING ` + campaignColumns
)

// LockTx locks the campaign row in tx (SELECT … FOR UPDATE) and returns it.
// It must be the first statement of every approval transition (§5.1).
func LockTx(ctx context.Context, tx pgx.Tx, id ID) (Campaign, error) {
	c, err := scanCampaign(tx.QueryRow(ctx, sqlSelectCampaignForUpdate, uuidArg(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign %s: lock: %w", id, err)
	}
	return c, nil
}

// GetTx reads the campaign in tx without locking it (consistent reads in a
// REPEATABLE READ transaction).
func GetTx(ctx context.Context, tx pgx.Tx, id ID) (Campaign, error) {
	c, err := scanCampaign(tx.QueryRow(ctx, sqlSelectCampaign, uuidArg(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign %s: select: %w", id, err)
	}
	return c, nil
}

// ContentTx reads one (re-validated) content version of the campaign in tx.
// Versions are immutable; it is used for the version an approval is bound
// to, which is not necessarily the current one.
func ContentTx(ctx context.Context, tx pgx.Tx, campaignID ID, contentID int64) (content.Version, error) {
	v, err := scanContent(tx.QueryRow(ctx, sqlSelectContent, contentID, uuidArg(campaignID)))
	if err != nil {
		return content.Version{}, fmt.Errorf("campaign %s: content %d: %w", campaignID, contentID, err)
	}
	return v, nil
}

// CurrentContentTx reads the campaign's current content version (re-validated)
// in a transaction that holds the campaign row lock, and checks that it is
// the newest version. nil if the campaign has no content.
func CurrentContentTx(ctx context.Context, tx pgx.Tx, cur Campaign) (*content.Version, error) {
	return currentContentTx(ctx, tx, cur)
}

// WriteApprovalStateTx writes the result of an approval transition (status,
// active_approval_id, version) of the locked campaign cur. next must differ
// from cur only in those fields and advance the version by exactly one.
func WriteApprovalStateTx(ctx context.Context, tx pgx.Tx, cur, next Campaign) (Campaign, error) {
	switch {
	case next.ID != cur.ID:
		return Campaign{}, fmt.Errorf("campaign %s: approval transition changed the id", cur.ID)
	case !next.Status.Valid():
		return Campaign{}, fmt.Errorf("campaign %s: invalid status %q", cur.ID, next.Status)
	case next.Version != cur.Version+1:
		return Campaign{}, fmt.Errorf("campaign %s: approval transition must advance the version by one", cur.ID)
	case !slices.Equal(next.Channels, cur.Channels) || next.SegmentVersion != cur.SegmentVersion ||
		!ptrEqual(next.CurrentContentID, cur.CurrentContentID):
		return Campaign{}, fmt.Errorf("campaign %s: approval transition changed authored fields", cur.ID)
	}
	var approval any
	if next.ActiveApprovalID != nil {
		approval = uuidArg(*next.ActiveApprovalID)
	}
	saved, err := scanCampaign(tx.QueryRow(ctx, sqlUpdateCampaignApproval,
		uuidArg(cur.ID), string(next.Status), approval, next.Version, cur.Version))
	if errors.Is(err, pgx.ErrNoRows) { // unreachable while the lock is held
		return Campaign{}, &ConflictError{Expected: cur.Version, Current: -1}
	}
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign %s: update approval state: %w", cur.ID, err)
	}
	return saved, nil
}
