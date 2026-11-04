package content

import (
	"errors"
	"time"
)

// Content versions (campaign_contents) are immutable: a row is inserted once
// and never updated. Every change of content text is a new version; the
// campaign points at its current version (campaigns.current_content_id) and
// an approval freezes one version by id (campaign_approvals.content_id).
// Because versions are immutable, binding the id binds the exact text the
// marketer approved; the renderer is deterministic, so the delivered text is
// a function of (version, frozen values) only.

// Origin is campaign_contents.origin.
type Origin string

// Origins.
const (
	OriginAI     Origin = "ai"
	OriginEdited Origin = "edited"
)

// Provenance records who produced a version. For AI versions the model and
// prompt version are required (model from configuration, never hardcoded);
// edited versions carry neither.
type Provenance struct {
	Origin        Origin `json:"origin"`
	Model         string `json:"model,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
}

// ErrInvalidProvenance is returned by Provenance.Validate.
var ErrInvalidProvenance = errors.New("content: invalid provenance")

// Validate checks the origin-dependent rules.
func (p Provenance) Validate() error {
	switch p.Origin {
	case OriginAI:
		if p.Model == "" || p.PromptVersion == "" {
			return errors.Join(ErrInvalidProvenance, errors.New("ai content requires model and prompt version"))
		}
	case OriginEdited:
		if p.Model != "" || p.PromptVersion != "" {
			return errors.Join(ErrInvalidProvenance, errors.New("edited content has no model or prompt version"))
		}
	default:
		return errors.Join(ErrInvalidProvenance, errors.New("unknown origin"))
	}
	return nil
}

// Version is one immutable content version. ID and CreatedAt are assigned by
// the database.
type Version struct {
	ID         int64
	Number     int // 1-based, per campaign
	Content    Content
	Provenance Provenance
	CreatedAt  time.Time
}

// ErrEmptyContent: a version must hold validated, non-empty content.
var ErrEmptyContent = errors.New("content: version without content")

// NextVersion decides whether saving c creates a new version after prev
// (nil when the campaign has no content yet) and returns it (ID and
// CreatedAt unset). Rules:
//
//   - An AI generation always creates a new version (it is an explicit
//     action with its own provenance).
//   - An edit whose canonical text is Equal to the current version creates
//     nothing (created=false): saving unchanged text must not revoke an
//     approval or bump versions.
//   - Any other change (any field's text or presence) creates a new version
//     numbered prev.Number+1.
func NextVersion(prev *Version, c Content, p Provenance) (v Version, created bool, err error) {
	if c.IsZero() {
		return Version{}, false, ErrEmptyContent
	}
	if err := p.Validate(); err != nil {
		return Version{}, false, err
	}
	number := 1
	if prev != nil {
		if p.Origin == OriginEdited && prev.Content.Equal(c) {
			return *prev, false, nil
		}
		number = prev.Number + 1
	}
	return Version{Number: number, Content: c, Provenance: p}, true, nil
}
