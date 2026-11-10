package attachments

import (
	"errors"
	"net/http"
	"os"
	"strings"
)

// CacheControl is the immutable cache policy for a content-addressed image.
// The URL is the digest, so a successful response never changes.
const CacheControl = "public, max-age=31536000, immutable"

// Handler serves GET /api/v1/attachments/{ref}. A ref that is not lowercase
// 64-hex, a digest with no row, and a row whose file is missing all answer
// 404. The handler does not echo the rejected ref.
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
		row, payload, err := store.Open(r.Context(), ref)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if row.OmittedReason != "" || len(payload) == 0 {
			http.NotFound(w, r)
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
