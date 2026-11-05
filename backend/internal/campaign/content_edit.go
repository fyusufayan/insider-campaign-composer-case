package campaign

import (
	"slices"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Operations of the content editor (CP13).
const (
	OpEditContent     Operation = "edit_content"
	OpValidateContent Operation = "validate_content"
)

// CodeChannelNotSelected: content was provided for a channel that is not
// selected. An error for edits (a version always covers exactly the
// selected channels when it is created), a warning when a stored version is
// shown after the selection changed.
const CodeChannelNotSelected = "channel_not_selected"

// CheckEditContent reports whether the marketer may save edited content.
// §5.2 allows a new content version in segmented … approved; as for
// generation (CheckGenerateContent), pending_approval and approved are
// refused until Slice 3 can revoke the live approval in the same
// transaction. Content is written for exactly the selected channels, so at
// least one channel must be selected.
func (c Campaign) CheckEditContent() error {
	switch c.Status {
	case StatusSegmented, StatusContentReady:
	case StatusDraft:
		return &StateError{Op: OpEditContent, Status: c.Status, Reason: "interpret the brief first"}
	case StatusPendingApproval, StatusApproved:
		return &StateError{Op: OpEditContent, Status: c.Status, Reason: reasonApprovalPending}
	default:
		return &StateError{Op: OpEditContent, Status: c.Status, Reason: "the campaign can no longer be edited"}
	}
	if c.ActiveApprovalID != nil { // impossible for a consistent row in these statuses
		return &StateError{Op: OpEditContent, Status: c.Status, Reason: reasonApprovalPending}
	}
	if len(c.Channels) == 0 {
		return &ValidationError{Problems: segment.Errors{{Path: "channels", Code: segment.CodeRequired,
			Message: "select at least one channel before writing content",
			Allowed: []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}}}}
	}
	return nil
}

// CheckValidateContent reports whether a draft may be validated (§11:
// segmented … approved). Validation is read-only, so the approval states
// are allowed.
func (c Campaign) CheckValidateContent() error {
	switch c.Status {
	case StatusSegmented, StatusContentReady, StatusPendingApproval, StatusApproved:
		return nil
	case StatusDraft:
		return &StateError{Op: OpValidateContent, Status: c.Status, Reason: "interpret the brief first"}
	default:
		return &StateError{Op: OpValidateContent, Status: c.Status, Reason: "the content can no longer be edited"}
	}
}

// ValidateDraft validates marketer input for the campaign's selected
// channels: content.Parse (the single content validator), then the rule
// that a draft has no content for an unselected channel, then the offer
// policy as advisory warnings. It is pure. On any error the Content is the
// zero value and the result has no rendered example.
func (c Campaign) ValidateDraft(d content.Draft) (content.Content, content.Result) {
	parsed, res := content.Parse(d, c.Channels)
	if !res.OK() {
		return content.Content{}, res
	}
	if extra := unselectedFields(parsed, c.Channels); len(extra) > 0 {
		res.Errors = append(res.Errors, extra...)
		res.RenderedExample = nil
		return content.Content{}, res
	}
	res.Warnings = append(res.Warnings, c.offerAdvisories(parsed)...)
	return parsed, res
}

// ValidateStored validates a stored version for the campaign's current
// selection (shown with the current content). The version is well-formed on
// its own (it was re-validated when read); missing text for a newly
// selected channel is an error, text for a channel that is no longer
// selected only a warning (it is never sent).
func (c Campaign) ValidateStored(v content.Version) content.Result {
	d := v.Content.Draft()
	extra := unselectedFields(v.Content, c.Channels)
	// Unselected fields are dropped before validating for the selection, so
	// they neither satisfy nor break the selected channels' requirements.
	for _, e := range extra {
		switch e.Path {
		case content.FieldSMSBody:
			d.SMSBody = nil
		case content.FieldEmailSubject:
			d.EmailSubject = nil
		case content.FieldEmailBody:
			d.EmailBody = nil
		}
	}
	for i := range extra {
		extra[i].Message = "the content has text for a channel that is not selected; it will not be sent"
	}
	if d.SMSBody == nil && d.EmailSubject == nil && d.EmailBody == nil {
		// Nothing left for the selection: report the missing fields.
		_, res := content.Parse(content.Draft{}, c.Channels)
		res.Warnings = append(res.Warnings, extra...)
		return res
	}
	parsed, res := content.Parse(d, c.Channels)
	res.Warnings = append(res.Warnings, extra...)
	if res.OK() {
		res.Warnings = append(res.Warnings, c.offerAdvisories(parsed)...)
	}
	return res
}

func (c Campaign) offerAdvisories(parsed content.Content) []segment.FieldError {
	brief, problems := ai.ParseContentBrief(c.ContentBrief)
	if len(problems) > 0 {
		return nil // no (valid) content brief: no authorized offer to compare with
	}
	return ai.OfferPolicyAdvisories(parsed, brief.Offer)
}

// unselectedFields reports the fields of parsed for channels outside chs.
func unselectedFields(parsed content.Content, chs []segment.Channel) segment.Errors {
	var out segment.Errors
	if parsed.Has(segment.ChannelSMS) && !slices.Contains(chs, segment.ChannelSMS) {
		out = append(out, segment.FieldError{Path: content.FieldSMSBody, Code: CodeChannelNotSelected,
			Message: "the sms channel is not selected; leave this field empty or select the channel"})
	}
	if parsed.Has(segment.ChannelEmail) && !slices.Contains(chs, segment.ChannelEmail) {
		for _, p := range []string{content.FieldEmailSubject, content.FieldEmailBody} {
			out = append(out, segment.FieldError{Path: p, Code: CodeChannelNotSelected,
				Message: "the email channel is not selected; leave this field empty or select the channel"})
		}
	}
	return out
}

// WithEditedContent applies a marketer edit made on top of expectedVersion.
// It is pure; the repository calls it under the campaign row lock.
//
//   - Stale edit: a campaign changed since expectedVersion → *ConflictError
//     (checked first: nothing is merged).
//   - The status must allow editing (CheckEditContent).
//   - The draft is validated for the selected channels (ValidateDraft);
//     problems → *ValidationError and nothing is written.
//   - content.NextVersion with origin edited: canonical text equal to the
//     current version is a no-op (created = false; version unchanged, no
//     row inserted); any change creates version n+1.
//   - On change: status → content_ready, version +1.
func (c Campaign) WithEditedContent(expectedVersion int, current *content.Version, d content.Draft) (Campaign, content.Version, bool, error) {
	if c.Version != expectedVersion {
		return Campaign{}, content.Version{}, false, &ConflictError{Expected: expectedVersion, Current: c.Version}
	}
	if err := c.CheckEditContent(); err != nil {
		return Campaign{}, content.Version{}, false, err
	}
	if err := c.checkCurrentContent(current); err != nil {
		return Campaign{}, content.Version{}, false, err
	}
	parsed, res := c.ValidateDraft(d)
	if !res.OK() {
		return Campaign{}, content.Version{}, false, &ValidationError{Problems: res.Errors}
	}
	v, created, err := content.NextVersion(current, parsed, content.Provenance{Origin: content.OriginEdited})
	if err != nil {
		return Campaign{}, content.Version{}, false, err
	}
	if !created {
		return c.clone(), v, false, nil
	}
	next := c.clone()
	next.Status = StatusContentReady
	next.Version++
	return next, v, true, nil
}
