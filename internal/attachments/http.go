package attachments

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
)

// CodeRefMalformed is the JSON error code for a path that is not a lowercase
// 64-hex sha256. The body does not repeat the rejected value.
const CodeRefMalformed = "attachment_ref_malformed"

// CodeNotFound is the JSON error code when a well-formed digest has no row,
// or the row exists and its file is missing.
const CodeNotFound = "attachment_not_found"

// CacheControl is the immutable cache policy for a content-addressed image.
// The URL is the digest, so a successful response never changes.
const CacheControl = "public, max-age=31536000, immutable"

// Handler serves GET /api/v1/attachments/{ref}. A ref that is not lowercase
// 64-hex answers 400 with code attachment_ref_malformed. A well-formed digest
// with no row, and a row whose file is missing, answer 404 with code
// attachment_not_found. The handler does not echo the rejected ref.
//
// The path value comes from the server mux (r.PathValue). A direct call that
// did not go through that mux falls back to the URL path so a unit test of
// this handler still names the digest.
func Handler(store *Store) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ref := r.PathValue("ref")
		if ref == "" {
			ref = strings.TrimPrefix(r.URL.Path, "/api/v1/attachments/")
			if i := strings.IndexByte(ref, '/'); i >= 0 {
				ref = ref[:i]
			}
		}
		if !validSHA(ref) {
			writeAttachmentError(w, http.StatusBadRequest, CodeRefMalformed, "The attachment request was refused in attachments.Handler because the path is not a lowercase 64-hex sha256, so no file was opened. Request the content address of a stored image.")
			return
		}
		row, payload, err := store.Open(r.Context(), ref)
		if err != nil || row.OmittedReason != "" || len(payload) == 0 {
			writeAttachmentError(w, http.StatusNotFound, CodeNotFound, "The attachment was not found in attachments.Handler because no stored row matches this content address, or the row exists and its file is missing. Re-index the session to restore the file, or request a digest that was stored.")
			return
		}
		w.Header().Set("Cache-Control", CacheControl)
		w.Header().Set("Content-Type", row.MediaType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})
}

// IsNotFound reports whether err is a missing attachment.
func IsNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

func writeAttachmentError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{Error: message, Code: code})
}
