// Package campaign owns the campaign aggregate: its fields, the campaign state
// machine (ARCHITECTURE §5.2), the repository and the service that the HTTP
// layer calls. It never talks to the LLM directly (only through
// ai.Interpreter) and never to the delivery provider.
package campaign

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Status is the campaign status (campaigns.status).
type Status string

// Statuses (§5.2).
const (
	StatusDraft                 Status = "draft"
	StatusSegmented             Status = "segmented"
	StatusContentReady          Status = "content_ready"
	StatusPendingApproval       Status = "pending_approval"
	StatusApproved              Status = "approved"
	StatusSending               Status = "sending"
	StatusCompleted             Status = "completed"
	StatusCompletedWithFailures Status = "completed_with_failures"
	StatusCancelled             Status = "cancelled"
)

var allStatuses = []Status{
	StatusDraft, StatusSegmented, StatusContentReady, StatusPendingApproval, StatusApproved,
	StatusSending, StatusCompleted, StatusCompletedWithFailures, StatusCancelled,
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool { return slices.Contains(allStatuses, s) }

// Field limits; identical to the campaigns CHECK constraints (§4). Lengths
// are counted in runes after NFC normalization and trimming.
const (
	MinNameRunes  = 1
	MaxNameRunes  = 120
	MinBriefRunes = ai.MinBriefRunes
	MaxBriefRunes = ai.MaxBriefRunes
)

// Field error codes specific to campaign input (the others are segment's).
const (
	CodeTooShort = "too_short"
)

// ---- ID ----

// ID is a campaign id (UUID).
type ID [16]byte

// ErrInvalidID is returned by ParseID.
var ErrInvalidID = errors.New("campaign: invalid id")

// ParseID accepts only the canonical 36-character UUID form
// (8-4-4-4-12 hexadecimal digits, either case).
func ParseID(s string) (ID, error) {
	var id ID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, ErrInvalidID
	}
	hexDigits := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(id[:], []byte(hexDigits)); err != nil {
		return ID{}, ErrInvalidID
	}
	return id, nil
}

// String returns the lower-case canonical form.
func (id ID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], id[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], id[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], id[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], id[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], id[10:16])
	return string(b[:])
}

// ---- Errors ----

var (
	// ErrNotFound: no campaign with the id.
	ErrNotFound = errors.New("campaign: not found")
	// ErrVersionConflict: the campaign changed since the version the caller
	// based its change on (optimistic concurrency).
	ErrVersionConflict = errors.New("campaign: version conflict")
	// ErrInvalidState: the operation is not allowed in the campaign's status.
	ErrInvalidState = errors.New("campaign: invalid state")
	// ErrInvalidInput: request values failed validation (*ValidationError).
	ErrInvalidInput = errors.New("campaign: invalid input")
	// ErrAudienceUnavailable: the audience query timed out (retryable).
	ErrAudienceUnavailable = errors.New("campaign: audience statistics unavailable")
)

// ValidationError lists field-level problems of caller input. Messages never
// echo the input.
type ValidationError struct {
	Problems segment.Errors
}

func (e *ValidationError) Error() string { return "campaign: invalid input: " + e.Problems.Error() }

// Is matches ErrInvalidInput.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidInput }

// ConflictError reports an optimistic-concurrency failure.
type ConflictError struct {
	// Expected is the version the caller based its change on; Current is
	// the stored version.
	Expected, Current int
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("campaign: version conflict (expected %d, current %d)", e.Expected, e.Current)
}

// Is matches ErrVersionConflict.
func (e *ConflictError) Is(target error) bool { return target == ErrVersionConflict }

// Operation names an operation checked against the state machine.
type Operation string

// Operations of Slice 1a.
const (
	OpInterpret   Operation = "interpret"
	OpSetChannels Operation = "set_channels"
	OpAudience    Operation = "audience"
)

// StateError reports an operation that the campaign status does not allow.
type StateError struct {
	Op     Operation
	Status Status
	// Reason is a fixed, display-safe explanation.
	Reason string
}

func (e *StateError) Error() string {
	return fmt.Sprintf("campaign: %s is not allowed in status %s: %s", e.Op, e.Status, e.Reason)
}

// Is matches ErrInvalidState.
func (e *StateError) Is(target error) bool { return target == ErrInvalidState }

// ---- Aggregate ----

// Campaign is the persisted campaign (the columns relevant to Slice 1a plus
// read-only references used by later slices).
type Campaign struct {
	ID     ID
	Name   string
	Brief  string
	Status Status
	// Channels is the marketer's selection in canonical order (sms, email);
	// never nil. An empty selection is a valid, explicit choice.
	Channels []segment.Channel
	// Segment is the validated, normalized rule; zero before the first
	// successful interpretation.
	Segment        segment.Validated
	SegmentVersion int
	// Interpretation is the persisted interpretation metadata (nil before
	// the first successful interpretation).
	Interpretation *ai.Record
	// ContentBrief is the persisted content brief JSON (nil before the first
	// successful interpretation). It is only ever written from a validated
	// ai.ContentBrief.
	ContentBrief     json.RawMessage
	CurrentContentID *int64
	ActiveApprovalID *ID
	// Version is the optimistic-concurrency version; +1 on every change.
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// clone returns a copy that shares no mutable memory with c.
func (c Campaign) clone() Campaign {
	c.Channels = append([]segment.Channel{}, c.Channels...)
	if c.Interpretation != nil {
		rec := *c.Interpretation
		rec.Assumptions = append([]string{}, rec.Assumptions...)
		rec.Unsupported = append([]string{}, rec.Unsupported...)
		c.Interpretation = &rec
	}
	if c.ContentBrief != nil {
		c.ContentBrief = append(json.RawMessage(nil), c.ContentBrief...)
	}
	if c.CurrentContentID != nil {
		v := *c.CurrentContentID
		c.CurrentContentID = &v
	}
	if c.ActiveApprovalID != nil {
		v := *c.ActiveApprovalID
		c.ActiveApprovalID = &v
	}
	return c
}

// Draft is validated input for a new campaign.
type Draft struct {
	Name  string
	Brief string
}

// NewDraft normalizes (NFC, trimmed) and validates a new campaign's name and
// brief. All problems are reported together.
func NewDraft(name, brief string) (Draft, error) {
	var errs segment.Errors
	d := Draft{Name: normalizeText(name), Brief: normalizeText(brief)}
	errs = checkText(errs, "name", d.Name, MinNameRunes, MaxNameRunes, false)
	errs = checkText(errs, "brief", d.Brief, MinBriefRunes, MaxBriefRunes, true)
	if len(errs) > 0 {
		return Draft{}, &ValidationError{Problems: errs}
	}
	return d, nil
}

func normalizeText(s string) string { return strings.TrimSpace(norm.NFC.String(s)) }

func checkText(errs segment.Errors, path, s string, minRunes, maxRunes int, multiline bool) segment.Errors {
	n := utf8.RuneCountInString(s)
	switch {
	case n == 0:
		return append(errs, segment.FieldError{Path: path, Code: segment.CodeEmpty, Message: "must not be empty"})
	case n < minRunes:
		return append(errs, segment.FieldError{Path: path, Code: CodeTooShort,
			Message: "must be at least " + strconv.Itoa(minRunes) + " characters"})
	case n > maxRunes:
		return append(errs, segment.FieldError{Path: path, Code: segment.CodeTooLong,
			Message: "must be at most " + strconv.Itoa(maxRunes) + " characters"})
	}
	bad := strings.IndexFunc(s, func(r rune) bool {
		if multiline && (r == '\n' || r == '\t') {
			return false
		}
		return unicode.IsControl(r)
	})
	if bad >= 0 {
		return append(errs, segment.FieldError{Path: path, Code: segment.CodeInvalidValue, Message: "must not contain control characters"})
	}
	return errs
}

// ---- State machine (§5.2, the events of Slice 1a) ----

// baseStatus is the status after an authoring change: content_ready when a
// content version exists, else segmented (or draft without a rule).
func (c Campaign) baseStatus() Status {
	switch {
	case c.CurrentContentID != nil:
		return StatusContentReady
	case !c.Segment.IsZero():
		return StatusSegmented
	default:
		return StatusDraft
	}
}

// reasonApprovalPending explains why edits in pending_approval/approved are
// refused until the approval package (Slice 3) can revoke the live approval
// and delete its held rows in the same transaction (invariant 6).
const reasonApprovalPending = "the campaign has a live approval; withdraw it first"

// CheckInterpret reports whether the brief may be (re-)interpreted.
// §5.2 allows draft … approved; pending_approval and approved additionally
// require revoking the live approval, which does not exist before Slice 3,
// so they are refused instead of leaving a stale approval behind.
func (c Campaign) CheckInterpret() error {
	switch c.Status {
	case StatusDraft, StatusSegmented, StatusContentReady:
		return nil
	case StatusPendingApproval, StatusApproved:
		return &StateError{Op: OpInterpret, Status: c.Status, Reason: reasonApprovalPending}
	default:
		return &StateError{Op: OpInterpret, Status: c.Status, Reason: "the campaign can no longer be edited"}
	}
}

// CheckSetChannels reports whether the channel selection may change
// (§5.2: segmented … approved; see CheckInterpret for the approval states).
func (c Campaign) CheckSetChannels() error {
	switch c.Status {
	case StatusSegmented, StatusContentReady:
		return nil
	case StatusDraft:
		return &StateError{Op: OpSetChannels, Status: c.Status, Reason: "interpret the brief first"}
	case StatusPendingApproval, StatusApproved:
		return &StateError{Op: OpSetChannels, Status: c.Status, Reason: reasonApprovalPending}
	default:
		return &StateError{Op: OpSetChannels, Status: c.Status, Reason: "the campaign can no longer be edited"}
	}
}

// CheckAudience reports whether a live audience preview may be computed
// (§11: segmented … approved). It is read-only, so the approval states are
// allowed.
func (c Campaign) CheckAudience() error {
	switch c.Status {
	case StatusSegmented, StatusContentReady, StatusPendingApproval, StatusApproved:
		if c.Segment.IsZero() { // impossible for a consistent row
			return &StateError{Op: OpAudience, Status: c.Status, Reason: "the campaign has no segment rule"}
		}
		return nil
	case StatusDraft:
		return &StateError{Op: OpAudience, Status: c.Status, Reason: "interpret the brief first"}
	default:
		return &StateError{Op: OpAudience, Status: c.Status, Reason: "the audience is no longer previewed in this status"}
	}
}

// WithInterpretation applies a successful interpretation that was computed
// from the campaign as it was at observedVersion. It is a pure function: the
// repository calls it while holding the campaign row lock.
//
//   - Stale result: if the campaign changed since observedVersion, the result
//     is discarded (*ConflictError), so AI output never overwrites a newer
//     edit.
//   - Rule: replaced by the new normalized rule; segment_version advances
//     only when the stored normalized rule actually changes.
//   - Channels (decision Q1): copied from suggested_channels only on the
//     first successful interpretation (segment_version = 0 and no channels).
//     Afterwards only SetChannels changes them; an explicitly empty selection
//     is kept.
//   - Status: draft → segmented; otherwise the base status.
//   - version: +1.
func (c Campaign) WithInterpretation(observedVersion int, in ai.Interpretation) (Campaign, error) {
	if c.Version != observedVersion {
		return Campaign{}, &ConflictError{Expected: observedVersion, Current: c.Version}
	}
	if err := c.CheckInterpret(); err != nil {
		return Campaign{}, err
	}
	if in.Segment.IsZero() {
		return Campaign{}, errors.New("campaign: interpretation without a segment rule")
	}
	changed, err := ruleChanged(c.Segment, in.Segment)
	if err != nil {
		return Campaign{}, err
	}
	brief, err := json.Marshal(in.ContentBrief)
	if err != nil {
		return Campaign{}, fmt.Errorf("campaign: encode content brief: %w", err)
	}
	next := c.clone()
	if changed {
		next.SegmentVersion++
	}
	if c.SegmentVersion == 0 && len(c.Channels) == 0 {
		chs, err := segment.NormalizeChannels(in.SuggestedChannels)
		if err != nil {
			return Campaign{}, fmt.Errorf("campaign: suggested channels: %w", err)
		}
		next.Channels = chs
	}
	next.Segment = in.Segment
	rec := in.Record()
	next.Interpretation = &rec
	next.ContentBrief = brief
	next.Status = next.baseStatus()
	next.Version++
	return next, nil
}

// WithChannels applies a channel selection made by the marketer on top of
// expectedVersion. The selection is normalized (de-duplicated, canonical
// order); an empty selection is valid. changed is false when the selection
// equals the stored one: nothing is written and the version does not move.
func (c Campaign) WithChannels(expectedVersion int, channels []segment.Channel) (next Campaign, changed bool, err error) {
	if c.Version != expectedVersion {
		return Campaign{}, false, &ConflictError{Expected: expectedVersion, Current: c.Version}
	}
	if err := c.CheckSetChannels(); err != nil {
		return Campaign{}, false, err
	}
	chs, err := segment.NormalizeChannels(channels)
	if err != nil {
		return Campaign{}, false, &ValidationError{Problems: segment.Errors{{
			Path: "channels", Code: segment.CodeNotAllowed, Message: "unknown channel",
			Allowed: []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}}}}
	}
	if slices.Equal(chs, c.Channels) {
		return c.clone(), false, nil
	}
	next = c.clone()
	next.Channels = chs
	next.Status = next.baseStatus()
	next.Version++
	return next, true, nil
}

// ruleChanged compares the canonical encodings of two normalized rules.
func ruleChanged(old, updated segment.Validated) (bool, error) {
	if old.IsZero() {
		return true, nil
	}
	a, err := json.Marshal(old)
	if err != nil {
		return false, fmt.Errorf("campaign: encode stored rule: %w", err)
	}
	b, err := json.Marshal(updated)
	if err != nil {
		return false, fmt.Errorf("campaign: encode new rule: %w", err)
	}
	return !bytes.Equal(a, b), nil
}
