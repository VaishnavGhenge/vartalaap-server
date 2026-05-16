package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// errorResponse is the canonical 4xx/5xx body shape. Clients gate UI behavior
// on Code (e.g. show an upgrade modal when Code == "FREE_PLAN_CAP") and fall
// back to Message when they don't have a translation for that Code yet.
//
// Field is set when the error is tied to a specific input — most validation
// errors. The browser uses it to highlight the right form control.
type errorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Field   string `json:"field,omitempty"`
}

// WriteJSON marshals v with the given status and the standard headers.
// Centralised so we never accidentally ship a 500 without a Content-Type.
//
// Exported (capitalised) because tests in adjacent files reach for it; the
// existing lower-case `writeJSON` is preserved as a shim during the refactor.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError emits a structured error body. Handlers should use this rather
// than http.Error so the UI receives a parseable Code instead of regexing the
// message. Plain message fallback is fine for unique, unrecoverable errors.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, errorResponse{Error: msg, Code: code})
}

// WriteFieldError is the FieldError-aware variant. Pulled out so the most
// common pattern — translating a validator return — is one call.
func WriteFieldError(w http.ResponseWriter, status int, err error) {
	var fe *FieldError
	if errors.As(err, &fe) {
		WriteJSON(w, status, errorResponse{
			Error:   fe.Message,
			Code:    fe.Code,
			Field:   fe.Field,
		})
		return
	}
	WriteError(w, status, "INVALID_REQUEST", err.Error())
}
