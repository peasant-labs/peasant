package defaults

// HeaderContentType is the HTTP Content-Type header key.
const HeaderContentType = "Content-Type"

// HeaderCacheControl is the HTTP Cache-Control header key.
const HeaderCacheControl = "Cache-Control"

// CacheControlNoStore is the Cache-Control value that forbids any caching of a
// response. Used for per-process answers (e.g. the UI capabilities
// advertisement) whose value can change across a server restart with different
// flags, so a cached answer would advertise stale capabilities.
const CacheControlNoStore = "no-store"

// ContentType is a typed HTTP content type value.
type ContentType string

func (c ContentType) String() string { return string(c) }

const (
	ContentJSON ContentType = "application/json"
	ContentHTML ContentType = "text/html"
)

// LocalhostAddrs is the set of addresses considered localhost for access control.
var LocalhostAddrs = []string{"127.0.0.1", "::1", "localhost"}
