package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/orshih6/billing/internal/models"
)

// HeaderIdempotencyKey makes a POST safe to retry.
const HeaderIdempotencyKey = "Idempotency-Key"

// idempotencyTTL is how long a key is remembered.
const idempotencyTTL = 24 * time.Hour

type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(code int) { c.status = code; c.ResponseWriter.WriteHeader(code) }

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(b)
	return c.ResponseWriter.Write(b)
}

type storedResponse struct {
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
}

// idempotent replays the stored response for a repeated POST with the same
// Idempotency-Key (scoped to the tenant). Reusing a key with a different
// request is an error; a request still in flight answers 409. Failed requests
// (5xx) are forgotten so they can be retried.
func (h *Handler) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderIdempotencyKey)
		if r.Method != http.MethodPost || key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > 200 {
			respondError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is limited to 200 characters")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid_body", "could not read request body")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])
		tenant := TenantFrom(r.Context())
		db := h.db.WithContext(r.Context())

		rec := models.IdempotencyRecord{
			TenantID: tenant, Key: key, Method: r.Method, Path: r.URL.Path,
			RequestHash: hash, CreatedAt: time.Now().UTC(),
		}
		// Forget expired keys lazily.
		db.Where("tenant_id = ? AND key = ? AND created_at < ?", tenant, key, time.Now().Add(-idempotencyTTL)).
			Delete(&models.IdempotencyRecord{})
		res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
		if res.Error != nil {
			respondServiceError(w, r, h.log, res.Error)
			return
		}
		if res.RowsAffected == 0 {
			var prev models.IdempotencyRecord
			if err := db.Where("tenant_id = ? AND key = ?", tenant, key).Take(&prev).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					respondError(w, http.StatusConflict, "idempotency_retry", "retry the request")
					return
				}
				respondServiceError(w, r, h.log, err)
				return
			}
			if prev.RequestHash != hash {
				respondError(w, http.StatusUnprocessableEntity, "idempotency_key_reused",
					"this Idempotency-Key was used with a different request")
				return
			}
			if prev.Status == 0 {
				respondError(w, http.StatusConflict, "idempotency_in_progress", "a request with this Idempotency-Key is still being processed")
				return
			}
			var stored storedResponse
			_ = json.Unmarshal(prev.Response, &stored)
			w.Header().Set("Content-Type", stored.ContentType)
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(prev.Status)
			if string(stored.Body) != "null" {
				_, _ = w.Write(stored.Body)
			}
			return
		}

		cw := &capture{ResponseWriter: w}
		next.ServeHTTP(cw, r)
		if cw.status >= 500 || cw.status == 0 {
			db.Where("tenant_id = ? AND key = ?", tenant, key).Delete(&models.IdempotencyRecord{})
			return
		}
		out := bytes.TrimSpace(cw.buf.Bytes())
		if len(out) == 0 {
			out = []byte("null")
		}
		payload, _ := json.Marshal(storedResponse{ContentType: w.Header().Get("Content-Type"), Body: json.RawMessage(out)})
		db.Model(&models.IdempotencyRecord{}).Where("tenant_id = ? AND key = ?", tenant, key).
			Updates(map[string]any{"status": cw.status, "response": payload})
	})
}
