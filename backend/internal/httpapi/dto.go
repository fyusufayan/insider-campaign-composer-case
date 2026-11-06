package httpapi

import (
	"encoding/json"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Response DTOs (mirrored by frontend/src/api/types.ts in CP7). Customer
// data (names, contacts, consents) never appears in any of them: audience
// statistics are aggregate counts only.

// campaignDTO is the campaign resource.
type campaignDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Brief  string `json:"brief"`
	Status string `json:"status"`
	// Channels: canonical order (sms, email); [] when none are selected.
	Channels []string `json:"channels"`
	// SegmentRule: the validated, normalized rule; null before the first
	// successful interpretation.
	SegmentRule    json.RawMessage `json:"segment_rule"`
	SegmentVersion int             `json:"segment_version"`
	// Interpretation: {summary, assumptions, unsupported, model,
	// prompt_version}; null before the first successful interpretation.
	Interpretation *ai.Record `json:"interpretation"`
	// ContentBrief: {goal, tone, length, offer}; null before the first
	// successful interpretation.
	ContentBrief json.RawMessage `json:"content_brief"`
	// CurrentContentID is the immutable content version the campaign points
	// at (null without content); GET /content returns it.
	CurrentContentID *int64 `json:"current_content_id"`
	// ActiveApprovalID is the pending/approved approval (or, once sending,
	// the consumed one); null otherwise. GET …/approvals/{id} reads it.
	ActiveApprovalID *string   `json:"active_approval_id"`
	Version          int       `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toCampaignDTO(c campaign.Campaign) (campaignDTO, error) {
	dto := campaignDTO{
		ID:               c.ID.String(),
		Name:             c.Name,
		Brief:            c.Brief,
		Status:           string(c.Status),
		Channels:         segment.ChannelNames(c.Channels),
		SegmentVersion:   c.SegmentVersion,
		Interpretation:   c.Interpretation,
		ContentBrief:     c.ContentBrief,
		Version:          c.Version,
		CurrentContentID: c.CurrentContentID,
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
	if c.ActiveApprovalID != nil {
		a := c.ActiveApprovalID.String()
		dto.ActiveApprovalID = &a
	}
	if !c.Segment.IsZero() {
		rule, err := json.Marshal(c.Segment)
		if err != nil {
			return campaignDTO{}, err
		}
		dto.SegmentRule = rule
	}
	return dto, nil
}

// interpretResponseDTO is the response of POST /campaigns/{id}/interpret.
// The interpretation is committed even if the audience could not be computed
// afterwards; then audience is null and audience_error explains why (the
// client can retry GET /campaigns/{id}/audience).
type interpretResponseDTO struct {
	Campaign      campaignDTO            `json:"campaign"`
	Audience      *segment.AudienceStats `json:"audience"`
	AudienceError *errorBody             `json:"audience_error,omitempty"`
}

// audienceResponseDTO is the response of GET /campaigns/{id}/audience: a
// live, informational preview (not a snapshot, nothing is frozen).
type audienceResponseDTO struct {
	CampaignID      string                `json:"campaign_id"`
	CampaignVersion int                   `json:"campaign_version"`
	SegmentVersion  int                   `json:"segment_version"`
	Channels        []string              `json:"channels"`
	Audience        segment.AudienceStats `json:"audience"`
}

// ---- Content (CP13) ----

// contentVersionDTO is one immutable content version (campaign_contents).
// Text fields are the canonical (NFC, trimmed) templates; null when the
// version has no text for the field.
type contentVersionDTO struct {
	ID            int64     `json:"id"`
	Version       int       `json:"version"`
	Origin        string    `json:"origin"`
	Model         *string   `json:"model"`
	PromptVersion *string   `json:"prompt_version"`
	CreatedAt     time.Time `json:"created_at"`
	SMSBody       *string   `json:"sms_body"`
	EmailSubject  *string   `json:"email_subject"`
	EmailBody     *string   `json:"email_body"`
}

func toContentVersionDTO(v content.Version) contentVersionDTO {
	d := v.Content.Draft()
	dto := contentVersionDTO{
		ID: v.ID, Version: v.Number, Origin: string(v.Provenance.Origin), CreatedAt: v.CreatedAt,
		SMSBody: d.SMSBody, EmailSubject: d.EmailSubject, EmailBody: d.EmailBody,
	}
	if v.Provenance.Model != "" {
		m := v.Provenance.Model
		dto.Model = &m
	}
	if v.Provenance.PromptVersion != "" {
		p := v.Provenance.PromptVersion
		dto.PromptVersion = &p
	}
	return dto
}

// lengthDTO is a field's worst-case length after personalization.
type lengthDTO struct {
	CharsWorstCase int `json:"chars_worst_case"`
	Limit          int `json:"limit"`
}

// renderedEmailDTO is a rendered email preview (the footer is appended by
// the backend and is not part of the body budget).
type renderedEmailDTO struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Footer  string `json:"footer"`
}

// renderedDTO is the rendered example with the fixed fictional values.
type renderedDTO struct {
	SMS   *string           `json:"sms"`
	Email *renderedEmailDTO `json:"email"`
}

// validationDTO is content.Result (ARCHITECTURE §8 result shape). errors
// and warnings are always arrays; the length statistics are null for a
// field that did not parse; rendered_example is null when there are errors.
type validationDTO struct {
	Valid           bool         `json:"valid"`
	Errors          []FieldError `json:"errors"`
	Warnings        []FieldError `json:"warnings"`
	SMS             *lengthDTO   `json:"sms"`
	Subject         *lengthDTO   `json:"subject"`
	Body            *lengthDTO   `json:"body"`
	RenderedExample *renderedDTO `json:"rendered_example"`
}

func toValidationDTO(r content.Result) validationDTO {
	l := func(s *content.LengthStats) *lengthDTO {
		if s == nil {
			return nil
		}
		return &lengthDTO{CharsWorstCase: s.CharsWorstCase, Limit: s.Limit}
	}
	dto := validationDTO{
		Valid: r.OK(), Errors: fieldErrors(r.Errors), Warnings: fieldErrors(r.Warnings),
		SMS: l(r.SMS), Subject: l(r.Subject), Body: l(r.Body),
	}
	if ex := r.RenderedExample; ex != nil {
		dto.RenderedExample = &renderedDTO{SMS: ex.SMS}
		if ex.Email != nil {
			dto.RenderedExample.Email = &renderedEmailDTO{Subject: ex.Email.Subject, Body: ex.Email.Body, Footer: ex.Email.Footer}
		}
	}
	return dto
}

// exampleValuesDTO are the fixed, fictional personalization values of every
// rendered example (content.ExampleValues). They are never customer data.
type exampleValuesDTO struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	City      string `json:"city"`
}

// contentLimitsDTO are the hard worst-case limits (runes after NFC).
type contentLimitsDTO struct {
	SMS          int `json:"sms_body"`
	EmailSubject int `json:"email_subject"`
	EmailBody    int `json:"email_body"`
}

func exampleValues() exampleValuesDTO {
	v := content.ExampleValues()
	return exampleValuesDTO{FirstName: v.FirstName, LastName: v.LastName, City: v.City}
}

func contentLimits() contentLimitsDTO {
	return contentLimitsDTO{SMS: content.MaxSMSRunes, EmailSubject: content.MaxEmailSubjectRunes, EmailBody: content.MaxEmailBodyRunes}
}

// currentContentDTO is GET /campaigns/{id}/content: the campaign's current
// content version (null if it has none — no draft is fabricated) and its
// validation for the selected channels, bound to campaign_version.
type currentContentDTO struct {
	CampaignID      string             `json:"campaign_id"`
	CampaignVersion int                `json:"campaign_version"`
	Channels        []string           `json:"channels"`
	Content         *contentVersionDTO `json:"content"`
	Validation      *validationDTO     `json:"validation"`
	ExampleValues   exampleValuesDTO   `json:"example_values"`
	Limits          contentLimitsDTO   `json:"limits"`
}

func toCurrentContentDTO(c campaign.Campaign, v *content.Version, res *content.Result) currentContentDTO {
	dto := currentContentDTO{
		CampaignID: c.ID.String(), CampaignVersion: c.Version, Channels: segment.ChannelNames(c.Channels),
		ExampleValues: exampleValues(), Limits: contentLimits(),
	}
	if v != nil {
		cv := toContentVersionDTO(*v)
		dto.Content = &cv
	}
	if res != nil {
		vd := toValidationDTO(*res)
		dto.Validation = &vd
	}
	return dto
}

// draftValidationDTO is POST /campaigns/{id}/content/validate: the
// read-only validation of a draft for the campaign state campaign_version.
type draftValidationDTO struct {
	CampaignID      string           `json:"campaign_id"`
	CampaignVersion int              `json:"campaign_version"`
	Channels        []string         `json:"channels"`
	Validation      validationDTO    `json:"validation"`
	ExampleValues   exampleValuesDTO `json:"example_values"`
	Limits          contentLimitsDTO `json:"limits"`
}

// contentSavedDTO is the response of PUT /campaigns/{id}/content and the
// data of the generation stream's "validated" event: the committed campaign
// and its current content. created = false: the canonical text was
// unchanged, nothing was written.
type contentSavedDTO struct {
	Campaign campaignDTO       `json:"campaign"`
	Content  currentContentDTO `json:"content"`
	Created  bool              `json:"created"`
	Notes    []string          `json:"notes"`
}
