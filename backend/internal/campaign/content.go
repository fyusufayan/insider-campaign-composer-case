package campaign

import (
	"errors"
	"fmt"
	"slices"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ErrContentNotFound: the campaign has no content version with the id.
var ErrContentNotFound = errors.New("campaign: content version not found")

// CheckGenerateContent reports whether AI content may be generated now.
//
// §5.2 allows a new content version in segmented … approved. In
// pending_approval and approved it must revoke the live approval and delete
// its held rows in the same transaction (invariant 6); that transition
// belongs to the approval package (Slice 3), so until then those states are
// refused rather than leaving an approval bound to outdated content. A
// campaign that is sending or terminal can never change. Generation also
// needs the stored interpretation (rule and content brief) and at least one
// selected channel: content is produced for exactly the selected channels,
// and an empty selection is a caller error (422 validation_failed), not a
// reason to generate useless content.
func (c Campaign) CheckGenerateContent() error {
	switch c.Status {
	case StatusSegmented, StatusContentReady:
	case StatusDraft:
		return &StateError{Op: OpGenerateContent, Status: c.Status, Reason: "interpret the brief first"}
	case StatusPendingApproval, StatusApproved:
		return &StateError{Op: OpGenerateContent, Status: c.Status, Reason: reasonApprovalPending}
	default:
		return &StateError{Op: OpGenerateContent, Status: c.Status, Reason: "the campaign can no longer be edited"}
	}
	if c.ActiveApprovalID != nil { // impossible for a consistent row in these statuses
		return &StateError{Op: OpGenerateContent, Status: c.Status, Reason: reasonApprovalPending}
	}
	if c.Segment.IsZero() || c.ContentBrief == nil { // impossible for a consistent row
		return &StateError{Op: OpGenerateContent, Status: c.Status, Reason: "the campaign has no interpretation"}
	}
	if len(c.Channels) == 0 {
		return &ValidationError{Problems: segment.Errors{{Path: "channels", Code: segment.CodeRequired,
			Message: "select at least one channel before generating content",
			Allowed: []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}}}}
	}
	return nil
}

// ContentMutation decides, while the campaign row lock is held, the next
// campaign state and the content version to insert. current is the
// campaign's current content version (nil if none), read in the same
// transaction. It must be pure (no I/O, invariant 13). created = false means
// nothing is written.
type ContentMutation func(cur Campaign, current *content.Version) (next Campaign, v content.Version, created bool, err error)

// WithAIContent applies generated content that was produced from the
// campaign as it was at observedVersion with the channel selection
// observedChannels. It is pure; the repository calls it under the campaign
// row lock.
//
//   - Stale result: if the campaign changed since observedVersion (any edit,
//     including a channel change or a concurrent generation), the result is
//     discarded (*ConflictError) and nothing is inserted.
//   - The status must still allow generation (CheckGenerateContent).
//   - The content must cover exactly the selected channels: the generator
//     never changes the selection and never invents content for a channel
//     that is not selected.
//   - content.NextVersion numbers the version (an AI generation always
//     creates one, even with identical text).
//   - Status → content_ready; version +1. current_content_id is set by the
//     repository to the inserted row in the same transaction.
func (c Campaign) WithAIContent(observedVersion int, observedChannels []segment.Channel, current *content.Version,
	gen content.Content, prov content.Provenance) (Campaign, content.Version, error) {
	if c.Version != observedVersion {
		return Campaign{}, content.Version{}, &ConflictError{Expected: observedVersion, Current: c.Version}
	}
	if err := c.CheckGenerateContent(); err != nil {
		return Campaign{}, content.Version{}, err
	}
	if !slices.Equal(c.Channels, observedChannels) { // implied by the version check
		return Campaign{}, content.Version{}, &ConflictError{Expected: observedVersion, Current: c.Version}
	}
	if !slices.Equal(gen.Channels(), c.Channels) {
		return Campaign{}, content.Version{}, fmt.Errorf("campaign %s: generated content does not match the selected channels", c.ID)
	}
	if prov.Origin != content.OriginAI {
		return Campaign{}, content.Version{}, fmt.Errorf("campaign %s: generated content must have origin ai", c.ID)
	}
	if err := c.checkCurrentContent(current); err != nil {
		return Campaign{}, content.Version{}, err
	}
	v, created, err := content.NextVersion(current, gen, prov)
	if err != nil {
		return Campaign{}, content.Version{}, fmt.Errorf("campaign %s: content version: %w", c.ID, err)
	}
	if !created { // unreachable: AI generations always create a version
		return Campaign{}, content.Version{}, fmt.Errorf("campaign %s: generation created no version", c.ID)
	}
	next := c.clone()
	next.Status = StatusContentReady
	next.Version++
	return next, v, nil
}

// checkCurrentContent asserts that current is the version the campaign
// points at.
func (c Campaign) checkCurrentContent(current *content.Version) error {
	switch {
	case c.CurrentContentID == nil && current == nil:
		return nil
	case c.CurrentContentID != nil && current != nil && current.ID == *c.CurrentContentID:
		return nil
	default:
		return fmt.Errorf("campaign %s: current content version is inconsistent", c.ID)
	}
}
