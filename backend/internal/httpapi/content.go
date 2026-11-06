package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Content endpoints (Slice 2, CP13):
//
//	GET  /api/v1/campaigns/{id}/content              current version + validation
//	POST /api/v1/campaigns/{id}/content/validate     validate a draft, read-only
//	PUT  /api/v1/campaigns/{id}/content              save an edit (new immutable version)
//	POST /api/v1/campaigns/{id}/content/generations  SSE: meta, status, delta*, validated | error
func (h *campaignHandlers) registerContent(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/campaigns/{id}/content", h.getContent)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/content/validate", h.validateContent)
	mux.HandleFunc("PUT /api/v1/campaigns/{id}/content", h.saveContent)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/content/generations", h.generateContent)
}

// contentFields are the draft properties (content.Draft DTO).
var contentFields = []string{content.FieldSMSBody, content.FieldEmailSubject, content.FieldEmailBody}

// decodeDraft reads the optional string-or-null draft fields.
func decodeDraft(b *body) content.Draft {
	return content.Draft{
		SMSBody:      b.optionalString(content.FieldSMSBody),
		EmailSubject: b.optionalString(content.FieldEmailSubject),
		EmailBody:    b.optionalString(content.FieldEmailBody),
	}
}

// GET /api/v1/campaigns/{id}/content → 200 current content (content: null
// when the campaign has none).
func (h *campaignHandlers) getContent(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	view, err := h.svc.Content(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toCurrentContentDTO(view.Campaign, view.Version, view.Validation))
}

// POST /api/v1/campaigns/{id}/content/validate {sms_body?, email_subject?,
// email_body?} → 200 validation. Malformed JSON or wrong property types are
// 422 validation_failed; well-formed JSON with invalid content is a 200
// whose validation lists the field errors. Nothing is written.
func (h *campaignHandlers) validateContent(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, false, contentFields...)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	d := decodeDraft(b)
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	v, err := h.svc.ValidateContent(r.Context(), id, d)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, draftValidationDTO{
		CampaignID: v.CampaignID.String(), CampaignVersion: v.CampaignVersion, Channels: segment.ChannelNames(v.Channels),
		Validation: toValidationDTO(v.Result), ExampleValues: exampleValues(), Limits: contentLimits(),
	})
}

// PUT /api/v1/campaigns/{id}/content {sms_body?, email_subject?,
// email_body?, expected_version} → 200 {campaign, content, created}.
// Stale expected_version → 409 version_conflict; invalid content → 422
// validation_failed with field details; unchanged text → created = false.
func (h *campaignHandlers) saveContent(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	b, apiErr := decodeBody(w, r, false, append([]string{"expected_version"}, contentFields...)...)
	if apiErr != nil {
		writeError(w, apiErr)
		return
	}
	d := decodeDraft(b)
	version, _ := b.positiveInt("expected_version")
	if apiErr := b.err(); apiErr != nil {
		writeError(w, apiErr)
		return
	}
	res, err := h.svc.EditContent(r.Context(), id, d, version)
	if err != nil {
		var valErr *campaign.ValidationError
		if errors.As(err, &valErr) {
			writeError(w, &APIError{Status: http.StatusUnprocessableEntity, Code: CodeValidationFailed,
				Message: "the content is invalid; nothing was saved", Details: fieldErrors(valErr.Problems)})
			return
		}
		h.fail(w, r, err)
		return
	}
	out, err := h.savedDTO(res)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *campaignHandlers) savedDTO(res campaign.ContentResult) (contentSavedDTO, error) {
	dto, err := toCampaignDTO(res.Campaign)
	if err != nil {
		return contentSavedDTO{}, err
	}
	v := res.Version
	val := res.Validation
	notes := res.Notes
	if notes == nil {
		notes = []string{}
	}
	return contentSavedDTO{Campaign: dto, Content: toCurrentContentDTO(res.Campaign, &v, &val), Created: res.Created, Notes: notes}, nil
}

// ---- Generation stream ----

// SSE event names of POST /content/generations (ARCHITECTURE §12).
const (
	sseEventMeta      = "meta"
	sseEventStatus    = "status"
	sseEventDelta     = "delta"
	sseEventValidated = "validated"
	sseEventError     = "error"
)

// Phases of the status event.
const (
	phaseGenerating = "generating" // a model request started (attempt, reason)
	phaseThinking   = "thinking"   // the model reasons before writing
	phaseValidating = "validating" // the attempt's complete output is validated
	phaseRepairing  = "repairing"  // it failed validation; a repair request follows
	phasePersisting = "persisting" // validated content is being committed
)

type sseMeta struct {
	CampaignID      string           `json:"campaign_id"`
	CampaignVersion int              `json:"campaign_version"`
	Channels        []string         `json:"channels"`
	Fields          []string         `json:"fields"`
	PromptVersion   string           `json:"prompt_version"`
	Limits          contentLimitsDTO `json:"limits"`
}

type sseStatus struct {
	Phase    string `json:"phase"`
	Attempt  int    `json:"attempt,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Problems int    `json:"problems,omitempty"`
}

// sseDelta is an unvalidated preview fragment; preview is always true so a
// client can never mistake it for saved content.
type sseDelta struct {
	Attempt int    `json:"attempt"`
	Field   string `json:"field"`
	Text    string `json:"text"`
	Preview bool   `json:"preview"`
}

type sseError struct {
	Status int       `json:"status"`
	Error  errorBody `json:"error"`
}

// generateContent streams one content generation as server-sent events.
//
// The service runs in its own goroutine and hands events to this goroutine
// over an unbuffered channel, which is the only writer of the response (plus
// heartbeat comments). Errors before the first event (not found, invalid
// state, no channels, malformed body) are ordinary JSON error responses.
// After the stream started, the outcome is exactly one terminal event:
// "validated" — sent only after the content version committed — or "error".
// A client disconnect (or a failed write) cancels the context, which
// cancels the model request; nothing is persisted unless the commit had
// already happened. No transaction is open while the model streams.
func (h *campaignHandlers) generateContent(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	type outcome struct {
		res campaign.ContentResult
		err error
	}
	events := make(chan campaign.GenerationEvent)
	done := make(chan outcome, 1)
	go func() {
		res, err := h.svc.GenerateContentStream(ctx, id, func(ev campaign.GenerationEvent) error {
			select {
			case events <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- outcome{res, err}
	}()

	sse := newSSEWriter(w)
	defer sse.finish()
	interval := h.heartbeat
	if interval <= 0 {
		interval = sseHeartbeat
	}
	heartbeat := time.NewTicker(interval)
	defer heartbeat.Stop()
	for {
		select {
		case ev := <-events:
			if err := h.writeGenerationEvent(sse, ev); err != nil {
				cancel() // client gone or unwritable: stop the model call
			}
		case <-heartbeat.C:
			if sse.started {
				if err := sse.comment("keep-alive"); err != nil {
					cancel()
				}
			}
		case out := <-done:
			h.finishGeneration(w, r, sse, out.res, out.err)
			return
		}
	}
}

func (h *campaignHandlers) writeGenerationEvent(sse *sseWriter, ev campaign.GenerationEvent) error {
	switch ev.Kind {
	case campaign.GenerationStarted:
		st := ev.Start
		fields := make([]string, 0, 3)
		for _, ch := range st.Channels {
			if ch == segment.ChannelSMS {
				fields = append(fields, content.FieldSMSBody)
			} else {
				fields = append(fields, content.FieldEmailSubject, content.FieldEmailBody)
			}
		}
		return sse.event(sseEventMeta, sseMeta{CampaignID: st.CampaignID.String(), CampaignVersion: st.CampaignVersion,
			Channels: segment.ChannelNames(st.Channels), Fields: fields, PromptVersion: st.PromptVersion, Limits: contentLimits()})
	case campaign.GenerationPersisting:
		return sse.event(sseEventStatus, sseStatus{Phase: phasePersisting})
	case campaign.GenerationProgress:
		p := ev.Progress
		switch p.Kind {
		case ai.ProgressAttempt:
			return sse.event(sseEventStatus, sseStatus{Phase: phaseGenerating, Attempt: p.Attempt, Reason: string(p.Reason)})
		case ai.ProgressThinking:
			return sse.event(sseEventStatus, sseStatus{Phase: phaseThinking, Attempt: p.Attempt})
		case ai.ProgressValidating:
			return sse.event(sseEventStatus, sseStatus{Phase: phaseValidating, Attempt: p.Attempt})
		case ai.ProgressRepair:
			return sse.event(sseEventStatus, sseStatus{Phase: phaseRepairing, Attempt: p.Attempt, Problems: p.Problems})
		case ai.ProgressDelta:
			for _, part := range splitRunes(p.Text, maxSSEDeltaBytes) {
				if err := sse.event(sseEventDelta, sseDelta{Attempt: p.Attempt, Field: p.Field, Text: part, Preview: true}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (h *campaignHandlers) finishGeneration(w http.ResponseWriter, r *http.Request, sse *sseWriter, res campaign.ContentResult, err error) {
	if err == nil {
		out, derr := h.savedDTO(res)
		if derr == nil {
			derr = sse.event(sseEventValidated, out)
		}
		switch {
		case derr == nil:
			return
		case sse.failed != nil:
			// The version is committed; the client left before the result
			// arrived and sees the content after a reload.
			h.log.InfoContext(r.Context(), "generated content committed but not delivered: client disconnected",
				"campaign_id", res.Campaign.ID.String(), "content_id", res.Version.ID)
			return
		default:
			// Encoding failed after the commit (a bug): report an error; the
			// client reloads the committed state.
			err = derr
		}
	}
	apiErr := h.toAPIErrorOp(r, err, opGenerateContent)
	if !sse.started {
		writeError(w, apiErr)
		return
	}
	if r.Context().Err() != nil || sse.failed != nil {
		return // nobody is listening
	}
	details := apiErr.Details
	if details == nil {
		details = []FieldError{}
	}
	_ = sse.event(sseEventError, sseError{Status: apiErr.Status, Error: errorBody{
		Code: apiErr.Code, Message: apiErr.Message, Details: details, Retryable: apiErr.Retryable,
		RawProposal: apiErr.RawProposal,
	}})
}
