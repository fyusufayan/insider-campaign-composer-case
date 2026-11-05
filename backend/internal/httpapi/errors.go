package httpapi

import (
	"encoding/json"
	"net/http"
)

// Error codes of the API error envelope (ARCHITECTURE §11). Codes for later
// slices are added together with the code paths that return them.
const (
	CodeValidationFailed     = "validation_failed"
	CodeNotFound             = "not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeInternal             = "internal"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeInvalidState         = "invalid_state"
	CodeVersionConflict      = "version_conflict"
	CodeAudienceUnavailable  = "audience_unavailable"
	CodeRequestCancelled     = "request_cancelled"
	// Approval codes (ARCHITECTURE §11).
	CodeApprovalExpired  = "approval_expired"
	CodeApprovalMismatch = "approval_mismatch"
	CodeEmptyAudience    = "empty_audience"
	CodeAudienceChanged  = "audience_changed"
	CodeCampaignBusy     = "campaign_busy"
	// CodeTooManyStreams: the progress-stream limit is reached (CP17B, retryable).
	CodeTooManyStreams = "too_many_streams"
	// AI codes; identical to ai.Code* (asserted by a test).
	CodeAIOutputInvalid = "ai_output_invalid"
	CodeLLMUnavailable  = "llm_unavailable"
	CodeLLMRejected     = "llm_rejected"
	CodeLLMRefused      = "llm_refused"
	CodeLLMTruncated    = "llm_truncated"
	CodeInvalidBrief    = "invalid_brief"
)

// FieldError is one entry of an error's details: a problem at a JSON path.
type FieldError struct {
	Path    string   `json:"path"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Allowed []string `json:"allowed,omitempty"`
}

// APIError is an error response: the HTTP status plus the envelope body.
type APIError struct {
	Status    int
	Code      string
	Message   string
	Details   []FieldError
	Retryable bool
	// RawProposal is the rejected model output of ai_output_invalid
	// (ARCHITECTURE §7.5). Untrusted text: clients display it escaped.
	RawProposal string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Details   []FieldError `json:"details"`
	Retryable bool         `json:"retryable"`
	// RawProposal is set only for ai_output_invalid.
	RawProposal string `json:"raw_proposal,omitempty"`
}

// writeError writes e as {"error": {"code", "message", "details", "retryable"}}.
// details is always an array, never null.
func writeError(w http.ResponseWriter, e *APIError) {
	details := e.Details
	if details == nil {
		details = []FieldError{}
	}
	writeJSON(w, e.Status, errorEnvelope{Error: errorBody{
		Code:        e.Code,
		Message:     e.Message,
		Details:     details,
		Retryable:   e.Retryable,
		RawProposal: e.RawProposal,
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The status is already sent; an encoding failure can only be a broken connection.
	_ = json.NewEncoder(w).Encode(v)
}
