package defaults

// HeaderContentType is the HTTP Content-Type header key.
const HeaderContentType = "Content-Type"

// ContentType is a typed HTTP content type value.
type ContentType string

func (c ContentType) String() string { return string(c) }

const (
	ContentJSON ContentType = "application/json"
	ContentHTML ContentType = "text/html"
)

// LocalhostAddrs is the set of addresses considered localhost for access control.
var LocalhostAddrs = []string{"127.0.0.1", "::1", "localhost"}
