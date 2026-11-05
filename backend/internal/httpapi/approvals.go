package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Approval endpoints (Slice 3, CP15; ARCHITECTURE §11, §11.3):
//
//	POST /api/v1/campaigns/{id}/approvals                   prepare {expected_version} → 201
//	GET  /api/v1/campaigns/{id}/approvals/{aid}             summary (lazy expiry first)
//	POST /api/v1/campaigns/{id}/approvals/{aid}/confirm     {expected_version, segment_version, content_version, channels}
//	POST /api/v1/campaigns/{id}/approvals/{aid}/withdraw    {expected_version}
//	POST /api/v1/campaigns/{id}/send                        {approval_id, expected_version} (CP16)
//
// Every response is {campaign, approval, server_time}. Responses carry
// counts, binding metadata, the content review (rendered with fictional
// example values) and offer findings — never recipient ids, names, cities,
// contacts or frozen personalization values.
type approvalHandlers struct {
	*campaignHandlers
	svc *approval.Service
}

func (h *approvalHandlers) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/campaigns/{id}/approvals", h.prepare)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/approvals/{aid}", h.get)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/approvals/{aid}/confirm", h.confirm)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/approvals/{aid}/withdraw", h.withdraw)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/send", h.send)
}

// POST /api/v1/campaigns/{id}/send {approval_id, expected_version} (CP16,
// §5.5 "Send", §11.4) → 202 {status, released, campaign, …} when this
// request consumed the approval and released its snapshot to the delivery
// queue; 200 with already_started = true for a repeat on a campaign that is
// already sending (or finished) with the same approval (idempotent; nothing
// written). Expired → 409 approval_expired; not confirmed / withdrawn /
// cancelled → 409 invalid_state; stale version → 409 version_conflict;
// binding drift → 409 approval_mismatch. The response carries counts only:
// no recipient ids, contacts, names or message text. No provider request
// is made by the API; the worker sends asynchronously.
func (h *approvalHandlers) send(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, false, "approval_id", "expected_version")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	raw, present := b.string("approval_id")
	version, _ := b.positiveInt("expected_version")
	var aid campaign.ID
	if present {
		var err error
		if aid, err = campaign.ParseID(raw); err != nil {
			b.add("approval_id", fieldInvalidValue, "must be a UUID")
		}
	}
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	out, err := h.svc.Send(r.Context(), id, aid, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	c, err := toCampaignDTO(out.Campaign)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dto := sendResponseDTO{Status: c.Status, ApprovalID: out.ApprovalID.String(), Released: map[string]int64{},
		AlreadyStarted: !out.Started, Campaign: c, ServerTime: time.Now().UTC()}
	for ch, n := range out.Released {
		dto.Released[string(ch)] = n
	}
	status := http.StatusAccepted
	if !out.Started {
		status = http.StatusOK
	}
	writeJSON(w, status, dto)
}

// sendResponseDTO is the response of POST /send. status is the campaign
// status (sending, or a finished status for a late repeat); released counts
// the delivery rows of the consumed approval per selected channel.
type sendResponseDTO struct {
	Status         string           `json:"status"`
	ApprovalID     string           `json:"approval_id"`
	Released       map[string]int64 `json:"released"`
	AlreadyStarted bool             `json:"already_started"`
	Campaign       campaignDTO      `json:"campaign"`
	ServerTime     time.Time        `json:"server_time"`
}

// POST /api/v1/campaigns/{id}/approvals {expected_version} → 201
// {campaign, approval}. Stale version → 409 version_conflict; wrong state →
// 409 invalid_state; content invalid for the selection → 422
// validation_failed; nobody eligible → 422 empty_audience.
func (h *approvalHandlers) prepare(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	version, ok := h.decodeExpectedVersion(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Prepare(r.Context(), id, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/campaigns/"+id.String()+"/approvals/"+out.Summary.ApprovalID.String())
	h.writeOutcome(w, r, http.StatusCreated, out)
}

// GET /api/v1/campaigns/{id}/approvals/{aid} → 200 {campaign, approval}.
func (h *approvalHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, aid, ok := h.pathIDs(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), id, aid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeOutcome(w, r, http.StatusOK, out)
}

// POST /api/v1/campaigns/{id}/approvals/{aid}/confirm {expected_version,
// segment_version, content_version, channels} → 200 {campaign, approval}.
// Expired → 409 approval_expired; binding differs → 409 approval_mismatch;
// stale version → 409 version_conflict; not pending → 409 invalid_state.
func (h *approvalHandlers) confirm(w http.ResponseWriter, r *http.Request) {
	id, aid, ok := h.pathIDs(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, false, "expected_version", "segment_version", "content_version", "channels")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	req := approval.ConfirmRequest{}
	req.ExpectedVersion, _ = b.positiveInt("expected_version")
	req.SegmentVersion, _ = b.positiveInt("segment_version")
	req.ContentVersion, _ = b.positiveInt("content_version")
	names, _ := b.stringArray("channels")
	allowed := []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}
	seen := map[segment.Channel]bool{}
	for i, n := range names {
		ch := segment.Channel(n)
		switch {
		case ch != segment.ChannelSMS && ch != segment.ChannelEmail:
			b.add("channels["+strconv.Itoa(i)+"]", fieldNotAllowed, "unknown channel", allowed...)
		case seen[ch]:
			b.add("channels["+strconv.Itoa(i)+"]", fieldDuplicate, "duplicate channel")
		default:
			seen[ch] = true
			req.Channels = append(req.Channels, ch)
		}
	}
	if _, present := b.fields["channels"]; present && len(names) == 0 && len(b.errs) == 0 {
		b.add("channels", fieldRequired, "at least one channel is required", allowed...)
	}
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	out, err := h.svc.Confirm(r.Context(), id, aid, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeOutcome(w, r, http.StatusOK, out)
}

// POST /api/v1/campaigns/{id}/approvals/{aid}/withdraw {expected_version}
// → 200 {campaign, approval}. Already withdrawn / consumed → 409
// invalid_state; expired → 409 approval_expired.
func (h *approvalHandlers) withdraw(w http.ResponseWriter, r *http.Request) {
	id, aid, ok := h.pathIDs(w, r)
	if !ok {
		return
	}
	version, ok := h.decodeExpectedVersion(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Withdraw(r.Context(), id, aid, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeOutcome(w, r, http.StatusOK, out)
}

// decodeExpectedVersion reads a strict {expected_version} body.
func (h *approvalHandlers) decodeExpectedVersion(w http.ResponseWriter, r *http.Request) (int, bool) {
	b, apiErr := decodeBody(w, r, false, "expected_version")
	if apiErr != nil {
		writeError(w, apiErr)
		return 0, false
	}
	version, _ := b.positiveInt("expected_version")
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return 0, false
	}
	return version, true
}

func (h *approvalHandlers) pathIDs(w http.ResponseWriter, r *http.Request) (campaign.ID, campaign.ID, bool) {
	id, ok := h.pathID(w, r)
	if !ok {
		return campaign.ID{}, campaign.ID{}, false
	}
	aid, err := campaign.ParseID(r.PathValue("aid"))
	if err != nil {
		writeError(w, &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
			Message: "approval id is invalid",
			Details: []FieldError{{Path: "aid", Code: fieldInvalidValue, Message: "must be a UUID"}}})
		return campaign.ID{}, campaign.ID{}, false
	}
	return id, aid, true
}

func (h *approvalHandlers) writeOutcome(w http.ResponseWriter, r *http.Request, status int, out approval.Outcome) {
	c, err := toCampaignDTO(out.Campaign)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, status, approvalResponseDTO{Campaign: c, Approval: toApprovalDTO(out.Summary), ServerTime: time.Now().UTC()})
}

// ---- DTOs ----

// approvalResponseDTO is the response of every approval endpoint.
// server_time lets the client align its expiry countdown with the server
// clock; the authoritative expiry decision is always the database's.
type approvalResponseDTO struct {
	Campaign   campaignDTO `json:"campaign"`
	Approval   approvalDTO `json:"approval"`
	ServerTime time.Time   `json:"server_time"`
}

// channelSnapshotDTO: the frozen recipients of one channel. render_issues
// counts held rows whose frozen values cannot be rendered (by code); null
// for a closed approval (its held rows were deleted).
type channelSnapshotDTO struct {
	Recipients   int64            `json:"recipients"`
	RenderIssues map[string]int64 `json:"render_issues"`
}

// approvalReviewDTO is the content review of a live approval: the bound
// content version re-validated for the bound channels against the current
// content brief. offer_findings are advisory (never blocking for marketer
// text) and are shown before confirmation.
type approvalReviewDTO struct {
	Validation    validationDTO `json:"validation"`
	OfferFindings []FieldError  `json:"offer_findings"`
	Offer         ai.Offer      `json:"offer"`
}

// approvalDTO is approval.Summary. It contains the documented prepare
// fields (approval_id, expires_at, segment_version, content_version,
// channels, audience_stats, rendered_content) plus the state needed to
// review, confirm or withdraw it.
type approvalDTO struct {
	ApprovalID           string                        `json:"approval_id"`
	CampaignID           string                        `json:"campaign_id"`
	Status               string                        `json:"status"`
	RevokeReason         *string                       `json:"revoke_reason"`
	CampaignVersion      int                           `json:"campaign_version"`
	SegmentVersion       int                           `json:"segment_version"`
	ContentID            int64                         `json:"content_id"`
	ContentVersion       int                           `json:"content_version"`
	ContentOrigin        string                        `json:"content_origin"`
	ContentModel         *string                       `json:"content_model"`
	ContentPromptVersion *string                       `json:"content_prompt_version"`
	Channels             []string                      `json:"channels"`
	AudienceStats        segment.AudienceStats         `json:"audience_stats"`
	SnapshotCount        int64                         `json:"snapshot_count"`
	SnapshotDigest       string                        `json:"snapshot_digest"`
	Snapshot             map[string]channelSnapshotDTO `json:"snapshot"`
	PreparedAt           time.Time                     `json:"prepared_at"`
	ExpiresAt            time.Time                     `json:"expires_at"`
	ApprovedAt           *time.Time                    `json:"approved_at"`
	ApprovedBy           *string                       `json:"approved_by"`
	ClosedAt             *time.Time                    `json:"closed_at"`
	Review               *approvalReviewDTO            `json:"review"`
	RenderedContent      *renderedDTO                  `json:"rendered_content"`
	SupersededApprovalID *string                       `json:"superseded_approval_id"`
}

func toApprovalDTO(s approval.Summary) approvalDTO {
	dto := approvalDTO{
		ApprovalID: s.ApprovalID.String(), CampaignID: s.CampaignID.String(), Status: string(s.Status),
		CampaignVersion: s.CampaignVersion, SegmentVersion: s.SegmentVersion,
		ContentID: s.ContentID, ContentVersion: s.ContentVersion, ContentOrigin: string(s.ContentProvenance.Origin),
		Channels: channelStrings(s.Channels), AudienceStats: s.AudienceStats,
		SnapshotCount: s.SnapshotCount, SnapshotDigest: s.SnapshotDigest, Snapshot: map[string]channelSnapshotDTO{},
		PreparedAt: s.PreparedAt, ExpiresAt: s.ExpiresAt, ApprovedAt: s.ApprovedAt, ClosedAt: s.ClosedAt,
	}
	if dto.AudienceStats.ByChannel == nil {
		dto.AudienceStats.ByChannel = map[segment.Channel]segment.ChannelStats{}
	}
	if s.RevokeReason != nil {
		rr := string(*s.RevokeReason)
		dto.RevokeReason = &rr
	}
	if m := s.ContentProvenance.Model; m != "" {
		dto.ContentModel = &m
	}
	if p := s.ContentProvenance.PromptVersion; p != "" {
		dto.ContentPromptVersion = &p
	}
	if s.ApprovedBy != "" {
		by := s.ApprovedBy
		dto.ApprovedBy = &by
	}
	for ch, cs := range s.Snapshot {
		cd := channelSnapshotDTO{Recipients: cs.Recipients}
		if cs.RenderIssues != nil {
			cd.RenderIssues = map[string]int64{}
			for k, n := range cs.RenderIssues {
				cd.RenderIssues[k] = n
			}
		}
		dto.Snapshot[string(ch)] = cd
	}
	if s.Reviewed {
		v := toValidationDTO(s.Review.Validation)
		dto.Review = &approvalReviewDTO{Validation: v, OfferFindings: fieldErrors(s.Review.OfferFindings), Offer: s.Review.Offer}
		dto.RenderedContent = v.RenderedExample
	}
	if s.Superseded != nil {
		id := s.Superseded.String()
		dto.SupersededApprovalID = &id
	}
	return dto
}
