package main

import "github.com/peasant-labs/schema"

// anonymousOwnerLabel is what the CLI prints for a person the village shows
// this viewer anonymously.
const anonymousOwnerLabel = "anon"

// ownerLabel renders a stored owner username for a table or header: "@name"
// for a profile, "anon" when the username is empty because the owner was
// anonymous to this viewer. It never prints "@" followed by an empty name.
func ownerLabel(username string) string {
	if username == "" {
		return anonymousOwnerLabel
	}
	return "@" + username
}

// publicOwnerLabel renders a person as the village showed them: their profile
// username, or "anon" for every other kind.
func publicOwnerLabel(owner schema.VillagePublicOwner) string {
	if !owner.IsProfile() {
		return anonymousOwnerLabel
	}
	return ownerLabel(owner.Username)
}

// storedOwnerJSON renders a stored owner username for JSON output: the name,
// or null when the owner was anonymous to this viewer, never an empty string.
func storedOwnerJSON(username string) *string {
	if username == "" {
		return nil
	}
	return &username
}
