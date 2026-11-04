package httpapi

import (
	"encoding/json"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
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
	Version      int             `json:"version"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

func toCampaignDTO(c campaign.Campaign) (campaignDTO, error) {
	dto := campaignDTO{
		ID:             c.ID.String(),
		Name:           c.Name,
		Brief:          c.Brief,
		Status:         string(c.Status),
		Channels:       channelStrings(c.Channels),
		SegmentVersion: c.SegmentVersion,
		Interpretation: c.Interpretation,
		ContentBrief:   c.ContentBrief,
		Version:        c.Version,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
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

func channelStrings(chs []segment.Channel) []string {
	out := make([]string, len(chs))
	for i, ch := range chs {
		out[i] = string(ch)
	}
	return out
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
