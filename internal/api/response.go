package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orshih6/billing/internal/billing"
)

// ErrorBody is every error response: {"error":{"code","message"}}.
type ErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

func respondJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func respondError(w http.ResponseWriter, status int, code, msg string) {
	var body ErrorBody
	body.Error.Code, body.Error.Message = code, msg
	respondJSON(w, status, body)
}

// respondServiceError maps a billing error to its status; anything else is a
// 500 whose details stay in the log.
func respondServiceError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	if e, ok := billing.AsError(err); ok {
		respondError(w, e.Status, e.Code, e.Message)
		return
	}
	log.Error("request failed", "error", err, "path", r.URL.Path, "request_id", RequestIDFrom(r.Context()))
	var body ErrorBody
	body.Error.Code, body.Error.Message = "internal_error", "internal server error"
	body.Error.RequestID = RequestIDFrom(r.Context())
	respondJSON(w, http.StatusInternalServerError, body)
}

// decode reads a JSON body strictly: unknown fields are an error, so a typo in
// a field name is reported instead of silently ignored.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		respondError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		respondError(w, http.StatusNotFound, "not_found", name+" is not a valid id")
		return uuid.Nil, false
	}
	return id, true
}

func queryUUID(w http.ResponseWriter, r *http.Request, name string) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_query", name+" is not a valid id")
		return nil, false
	}
	return &id, true
}

func page(w http.ResponseWriter, r *http.Request) (billing.Page, bool) {
	var p billing.Page
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			respondError(w, http.StatusBadRequest, "invalid_query", "limit must be a positive integer")
			return p, false
		}
		p.Limit = n
	}
	after, ok := queryUUID(w, r, "starting_after")
	p.StartingAfter = after
	return p, ok
}

// common reads the filters every list accepts:
//
//	created_after=2026-01-01  created_before=2026-02-01T00:00:00Z  metadata[plan]=gold
func common(w http.ResponseWriter, r *http.Request) (billing.Common, bool) {
	var c billing.Common
	parse := func(name string) (*time.Time, bool) {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return nil, true
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if t, err := time.Parse(layout, raw); err == nil {
				return &t, true
			}
		}
		respondError(w, http.StatusBadRequest, "invalid_query", name+" must be a date (2026-01-31) or RFC 3339 time")
		return nil, false
	}
	var ok bool
	if c.CreatedAfter, ok = parse("created_after"); !ok {
		return c, false
	}
	if c.CreatedBefore, ok = parse("created_before"); !ok {
		return c, false
	}
	for k, v := range r.URL.Query() {
		if key, found := strings.CutPrefix(k, "metadata["); found && strings.HasSuffix(key, "]") && len(v) > 0 {
			if c.Metadata == nil {
				c.Metadata = map[string]string{}
			}
			c.Metadata[strings.TrimSuffix(key, "]")] = v[0]
		}
	}
	return c, true
}

func queryBool(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}
