package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// statusClientClosedRequest is used (and logged) when the client went away;
// nobody reads the response.
const statusClientClosedRequest = 499

// campaignHandlers serve the campaign endpoints of Slice 1a. Each handler
// decodes the request, calls one service method and encodes the result.
type campaignHandlers struct {
	svc *campaign.Service
	log *slog.Logger
	// heartbeat is the SSE keep-alive interval (sseHeartbeat; tests shorten it).
	heartbeat time.Duration
}

func (h *campaignHandlers) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/campaigns", h.create)
	mux.HandleFunc("GET /api/v1/campaigns/{id}", h.get)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/interpret", h.interpret)
	mux.HandleFunc("GET /api/v1/campaigns/{id}/audience", h.audience)
	mux.HandleFunc("PUT /api/v1/campaigns/{id}/channels", h.setChannels)
	h.registerContent(mux)
}

// POST /api/v1/campaigns {name, brief} → 201 campaign.
func (h *campaignHandlers) create(w http.ResponseWriter, r *http.Request) {
	b, apiErr := decodeBody(w, r, false, "name", "brief")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	name, _ := b.string("name")
	brief, _ := b.string("brief")
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	c, err := h.svc.Create(r.Context(), name, brief)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/v1/campaigns/"+c.ID.String())
	h.writeCampaign(w, r, http.StatusCreated, c)
}

// GET /api/v1/campaigns/{id} → 200 campaign.
func (h *campaignHandlers) get(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	c, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeCampaign(w, r, http.StatusOK, c)
}

// POST /api/v1/campaigns/{id}/interpret (body empty or {}) →
// 200 {campaign, audience, audience_error?}.
func (h *campaignHandlers) interpret(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, true)
	if apiErr == nil {
		apiErr = b.err()
	}
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	res, err := h.svc.Interpret(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	dto, err := toCampaignDTO(res.Campaign)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := interpretResponseDTO{Campaign: dto, Audience: res.Audience}
	if res.AudienceErr != nil {
		e := h.toAPIError(r, res.AudienceErr)
		out.AudienceError = &errorBody{Code: e.Code, Message: e.Message, Details: []FieldError{}, Retryable: e.Retryable}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/v1/campaigns/{id}/audience → 200 live preview.
func (h *campaignHandlers) audience(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	p, err := h.svc.Audience(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, audienceResponseDTO{
		CampaignID:      p.CampaignID.String(),
		CampaignVersion: p.CampaignVersion,
		SegmentVersion:  p.SegmentVersion,
		Channels:        channelStrings(p.Channels),
		Audience:        p.Stats,
	})
}

// PUT /api/v1/campaigns/{id}/channels {channels, expected_version} → 200 campaign.
func (h *campaignHandlers) setChannels(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, false, "channels", "expected_version")
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	names, _ := b.stringArray("channels")
	version, _ := b.positiveInt("expected_version")
	allowed := []string{string(segment.ChannelSMS), string(segment.ChannelEmail)}
	chs := make([]segment.Channel, 0, len(names))
	for i, n := range names {
		ch := segment.Channel(n)
		if ch != segment.ChannelSMS && ch != segment.ChannelEmail {
			b.add("channels["+strconv.Itoa(i)+"]", fieldNotAllowed, "unknown channel", allowed...)
			continue
		}
		chs = append(chs, ch)
	}
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	c, err := h.svc.SetChannels(r.Context(), id, chs, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeCampaign(w, r, http.StatusOK, c)
}

func (h *campaignHandlers) pathID(w http.ResponseWriter, r *http.Request) (campaign.ID, bool) {
	id, err := campaign.ParseID(r.PathValue("id"))
	if err != nil {
		writeError(w, &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
			Message: "campaign id is invalid",
			Details: []FieldError{{Path: "id", Code: fieldInvalidValue, Message: "must be a UUID"}}})
		return campaign.ID{}, false
	}
	return id, true
}

func (h *campaignHandlers) writeCampaign(w http.ResponseWriter, r *http.Request, status int, c campaign.Campaign) {
	dto, err := toCampaignDTO(c)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, status, dto)
}

func (h *campaignHandlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, h.toAPIError(r, err))
}

// toAPIError maps domain errors to the error envelope. Unknown errors become
// a generic 500 whose cause is logged, never returned: messages sent to the
// client are fixed texts and never contain SQL, connection strings, secrets,
// raw provider errors or customer data.
func (h *campaignHandlers) toAPIError(r *http.Request, err error) *APIError {
	return h.toAPIErrorOp(r, err, opInterpret)
}

// aiOperation selects the wording of AI error messages.
type aiOperation int

const (
	opInterpret aiOperation = iota
	opGenerateContent
)

func (h *campaignHandlers) toAPIErrorOp(r *http.Request, err error, op aiOperation) *APIError {
	ctx := r.Context()
	logAttrs := []any{"err", err}
	if id, perr := campaign.ParseID(r.PathValue("id")); perr == nil {
		logAttrs = append(logAttrs, "campaign_id", id.String())
	}
	if e, ok := ai.AsError(err); ok {
		if op == opGenerateContent {
			return contentAIAPIError(e)
		}
		return aiAPIError(e)
	}
	var (
		valErr      *campaign.ValidationError
		stateErr    *campaign.StateError
		conflictErr *campaign.ConflictError
	)
	switch {
	case errors.As(err, &valErr):
		return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
			Message: "request is invalid", Details: fieldErrors(valErr.Problems)}
	case errors.Is(err, campaign.ErrNotFound):
		return &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: "campaign not found"}
	case errors.As(err, &conflictErr):
		return &APIError{Status: http.StatusConflict, Code: CodeVersionConflict,
			Message: "the campaign was changed by another request; reload it and try again"}
	case errors.As(err, &stateErr):
		return &APIError{Status: http.StatusConflict, Code: CodeInvalidState,
			Message: string(stateErr.Op) + " is not allowed while the campaign is " + string(stateErr.Status) + ": " + stateErr.Reason}
	case errors.Is(err, campaign.ErrAudienceUnavailable):
		h.log.WarnContext(ctx, "audience statistics unavailable", logAttrs...)
		return &APIError{Status: http.StatusServiceUnavailable, Code: CodeAudienceUnavailable,
			Message: "audience statistics are temporarily unavailable; try again", Retryable: true}
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		h.log.InfoContext(ctx, "request cancelled by the client")
		return &APIError{Status: statusClientClosedRequest, Code: CodeRequestCancelled, Message: "request cancelled"}
	default:
		h.log.ErrorContext(ctx, "request failed", logAttrs...)
		return &APIError{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "internal server error"}
	}
}

// aiAPIError maps interpretation failures (ARCHITECTURE §7.5). Anthropic
// failures are surfaced as errors; nothing falls back to demo output.
func aiAPIError(e *ai.Error) *APIError {
	switch e.Kind {
	case ai.ErrLLMUnavailable:
		return &APIError{Status: http.StatusServiceUnavailable, Code: CodeLLMUnavailable,
			Message: "the AI service is unavailable or timed out; try again later", Retryable: e.Retryable}
	case ai.ErrLLMRejected:
		return &APIError{Status: http.StatusBadGateway, Code: CodeLLMRejected,
			Message: "the AI service rejected the request; check the server configuration"}
	case ai.ErrLLMRefused:
		return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeLLMRefused,
			Message: "the AI model declined to interpret this brief; rephrase the brief and try again"}
	case ai.ErrLLMTruncated:
		return &APIError{Status: http.StatusBadGateway, Code: CodeLLMTruncated,
			Message: "the AI answer was incomplete (output limit reached); shorten the brief or try again"}
	case ai.ErrOutputInvalid:
		return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeAIOutputInvalid,
			Message:     "the AI proposal was invalid after one repair attempt; nothing was saved",
			Details:     fieldErrors(e.Details),
			RawProposal: e.RawProposal}
	case ai.ErrInvalidBrief:
		details := fieldErrors(e.Details)
		if len(details) == 0 {
			details = []FieldError{{Path: "brief", Code: fieldInvalidValue, Message: "the brief cannot be interpreted"}}
		}
		return &APIError{Status: http.StatusUnprocessableEntity, Code: CodeInvalidBrief,
			Message: "the brief cannot be turned into a supported audience rule; edit the brief", Details: details}
	default:
		return &APIError{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "internal server error"}
	}
}

// contentAIAPIError maps content generation failures (§7.5) with messages
// about content rather than the brief. Nothing is saved in any case.
func contentAIAPIError(e *ai.Error) *APIError {
	apiErr := aiAPIError(e)
	switch e.Kind {
	case ai.ErrLLMRefused:
		apiErr.Message = "the AI model declined to write this content; write it manually or adjust the brief"
	case ai.ErrLLMTruncated:
		apiErr.Message = "the AI answer was incomplete (output limit reached); nothing was saved, try again"
	case ai.ErrOutputInvalid:
		apiErr.Message = "the generated content was invalid after one repair attempt; nothing was saved"
	case ai.ErrLLMUnavailable:
		apiErr.Message = "the AI service is unavailable or timed out; nothing was saved, try again later"
	}
	return apiErr
}

func fieldErrors(errs segment.Errors) []FieldError {
	out := make([]FieldError, len(errs))
	for i, e := range errs {
		out[i] = FieldError{Path: e.Path, Code: e.Code, Message: e.Message, Allowed: e.Allowed}
	}
	return out
}
