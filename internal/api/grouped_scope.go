package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

const (
	// groupedMembersDefaultLimit bounds one member page when the caller omits
	// limit. It matches the existing list default so the chooser and the list
	// page in the same units.
	groupedMembersDefaultLimit = 20
	// groupedMembersMaxLimit caps one member page. A caller requesting more is
	// clamped rather than refused, matching the existing search limit behavior.
	groupedMembersMaxLimit = 50
	// memberScopeTTL is the bounded lifetime of an opaque member scope. Fifteen
	// minutes is long enough to open a disclosure and short enough that a stale
	// scope is refreshed rather than silently reused.
	memberScopeTTL = 15 * time.Minute
	// memberScopeMaxEntries bounds cache memory on a busy installation. The
	// oldest-expiring entry is evicted first, and an evicted scope behaves like
	// an expired one: the caller refreshes the originating list.
	memberScopeMaxEntries = 2048
)

// memberScopeEntry is the server-local record behind an opaque member scope. It
// is a lookup token, NOT an access grant: every replay re-applies the current
// route predicate, authorization, selection and eligibility.
type memberScopeEntry struct {
	GroupID   string
	Variant   GroupedRouteVariant
	Filters   GroupedFilters
	Revision  string
	ExpiresAt time.Time
}

// memberScopeCache is a bounded server-local TTL cache of member scopes. Tokens
// are 128 bits of cryptographic randomness and carry no session identifiers, so
// a leaked token reveals nothing beyond its own short-lived lookup.
type memberScopeCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[string]memberScopeEntry
}

func newMemberScopeCache(ttl time.Duration, maxEntries int) *memberScopeCache {
	return &memberScopeCache{
		ttl:     ttl,
		max:     maxEntries,
		now:     time.Now,
		entries: make(map[string]memberScopeEntry),
	}
}

// randomMemberScopeToken returns 128 bits of cryptographic randomness as hex.
func randomMemberScopeToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("api.randomMemberScopeToken: read randomness for a grouped member scope while a grouped list was being served: %w; the scope was not issued, so the helper group cannot be expanded; retry the grouped list", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// issue records a scope for one group and returns its opaque token.
func (c *memberScopeCache) issue(groupID string, variant GroupedRouteVariant, filters GroupedFilters, revision string) (string, error) {
	token, err := randomMemberScopeToken()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.evictExpiredLocked(now)
	if c.max > 0 && len(c.entries) >= c.max {
		c.evictOldestLocked()
	}
	c.entries[token] = memberScopeEntry{
		GroupID:   groupID,
		Variant:   variant,
		Filters:   filters,
		Revision:  revision,
		ExpiresAt: now.Add(c.ttl),
	}
	return token, nil
}

// lookup returns the live record for a token, or false when the token is
// unknown or expired. An expired entry is removed so the cache does not grow
// without bound.
func (c *memberScopeCache) lookup(token string) (memberScopeEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[token]
	if !ok {
		return memberScopeEntry{}, false
	}
	if !c.now().Before(entry.ExpiresAt) {
		delete(c.entries, token)
		return memberScopeEntry{}, false
	}
	return entry, true
}

func (c *memberScopeCache) evictExpiredLocked(now time.Time) {
	for token, entry := range c.entries {
		if !now.Before(entry.ExpiresAt) {
			delete(c.entries, token)
		}
	}
}

func (c *memberScopeCache) evictOldestLocked() {
	var oldestToken string
	var oldest time.Time
	first := true
	for token, entry := range c.entries {
		if first || entry.ExpiresAt.Before(oldest) {
			oldestToken, oldest, first = token, entry.ExpiresAt, false
		}
	}
	if !first {
		delete(c.entries, oldestToken)
	}
}

// issueMemberScope satisfies the grouping issuer seam with the server's cache
// and the process selection revision.
func (s *Server) issueMemberScope(groupID string, variant GroupedRouteVariant, filters GroupedFilters) (string, error) {
	return s.memberScopes.issue(groupID, variant, filters, s.groupedRevision)
}

// lookupMemberScope returns the live member scope record for a token.
func (s *Server) lookupMemberScope(token string) (memberScopeEntry, bool) {
	if s.memberScopes == nil {
		return memberScopeEntry{}, false
	}
	return s.memberScopes.lookup(token)
}
