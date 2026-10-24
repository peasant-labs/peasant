package village

import (
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/schema"
)

// --- Reading a person on the pull surface, in either supported envelope ---
//
// Pull contract 0.3.0 carries the transcript owner and the annotation author as
// a schema.VillagePublicOwner the village resolves for the viewer: a public
// profile, or anonymous. Pull contract 0.2.0 carried the account strings
// ownerUserId/ownerUsername and authorUserId/authorUsername, which always named
// the person. This CLI reads both, so it keeps pulling from a village that has
// not moved to 0.3.0 yet, and maps each into the one internal representation:
// the 0.2.0 strings become a profile, because the 0.2.0 village showed every
// person.
//
// Every reader of a stored pull metadata snapshot or annotation payload goes
// through the same decoders, so a file written before this CLI was upgraded
// reads back as the same person it was pulled as.

// PublicOwnerForReader returns the person as this CLI may show them. A profile
// is kept as it is; every other value, including a kind this contract version
// does not know and a zero value, becomes the anonymous shape and keeps no
// field. It is the single place the "unknown kind is anonymous" rule is applied
// before the pull pipeline stores or re-encodes a person.
func PublicOwnerForReader(owner schema.VillagePublicOwner) schema.VillagePublicOwner {
	if owner.IsProfile() {
		return owner
	}
	return schema.AnonymousVillageOwner()
}

// legacyPublicOwner maps the 0.2.0 account strings into the internal
// representation. The 0.2.0 village showed every person, so the strings are a
// profile; strings that do not form a valid profile (no canonical id, or no
// username) are read as anonymous rather than trusted as an identity.
func legacyPublicOwner(id string, username *string) schema.VillagePublicOwner {
	name := ""
	if username != nil {
		name = *username
	}
	return PublicOwnerForReader(schema.NewVillagePublicProfile(schema.VillageUUID(id), name, nil, nil))
}

// resolvePullPerson selects the person from whichever envelope arrived. The
// 0.3.0 field wins when both are present, so a stray account string never
// overrides an anonymous value. A row carrying neither shape is refused.
func resolvePullPerson(current json.RawMessage, legacyID, legacyUsername *string, currentField, legacyField string) (schema.VillagePublicOwner, error) {
	if current != nil {
		var owner schema.VillagePublicOwner
		if err := json.Unmarshal(current, &owner); err != nil {
			return schema.VillagePublicOwner{}, fmt.Errorf("read %s: %w", currentField, err)
		}
		return PublicOwnerForReader(owner), nil
	}
	if legacyID != nil {
		return legacyPublicOwner(*legacyID, legacyUsername), nil
	}
	return schema.VillagePublicOwner{}, fmt.Errorf("read pull envelope: neither %s (pull contract 0.3.0) nor %s (pull contract 0.2.0) is present; peasant online sent a shape this CLI does not read", currentField, legacyField)
}

// pullTranscriptInfoWire reads one PullTranscriptInfo in either envelope. The
// outer Owner shadows the embedded one, so the 0.3.0 field arrives raw and is
// resolved together with the 0.2.0 strings.
type pullTranscriptInfoWire struct {
	schema.PullTranscriptInfo
	Owner         json.RawMessage `json:"owner"`
	OwnerUserID   *string         `json:"ownerUserId"`
	OwnerUsername *string         `json:"ownerUsername"`
}

func (w *pullTranscriptInfoWire) info() (schema.PullTranscriptInfo, error) {
	owner, err := resolvePullPerson(w.Owner, w.OwnerUserID, w.OwnerUsername, "owner", "ownerUserId")
	if err != nil {
		return schema.PullTranscriptInfo{}, fmt.Errorf("transcript %s: %w", w.TranscriptID, err)
	}
	info := w.PullTranscriptInfo
	info.Owner = owner
	return info, nil
}

// pullAnnotationWire reads one PullAnnotation in either envelope.
type pullAnnotationWire struct {
	schema.PullAnnotation
	Author         json.RawMessage `json:"author"`
	AuthorUserID   *string         `json:"authorUserId"`
	AuthorUsername *string         `json:"authorUsername"`
}

func (w *pullAnnotationWire) annotation() (schema.PullAnnotation, error) {
	author, err := resolvePullPerson(w.Author, w.AuthorUserID, w.AuthorUsername, "author", "authorUserId")
	if err != nil {
		return schema.PullAnnotation{}, fmt.Errorf("annotation %s: %w", w.ID, err)
	}
	annotation := w.PullAnnotation
	annotation.Author = author
	return annotation, nil
}

type pullListResponseWire struct {
	Transcripts []pullTranscriptInfoWire `json:"transcripts"`
	Page        int                      `json:"page"`
	Limit       int                      `json:"limit"`
	Total       int                      `json:"total"`
}

// DecodePullTranscriptInfo reads one pull transcript metadata view, served by
// the village or stored as a pull metadata snapshot, in either supported
// envelope.
func DecodePullTranscriptInfo(data []byte) (schema.PullTranscriptInfo, error) {
	var wire pullTranscriptInfoWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return schema.PullTranscriptInfo{}, err
	}
	return wire.info()
}

// DecodePullAnnotation reads one pull annotation, served by the village or
// stored as a pulled annotation payload, in either supported envelope.
func DecodePullAnnotation(data []byte) (schema.PullAnnotation, error) {
	var wire pullAnnotationWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return schema.PullAnnotation{}, err
	}
	return wire.annotation()
}

// decodePullAnnotations reads the annotations endpoint's bare array.
func decodePullAnnotations(data []byte) ([]schema.PullAnnotation, error) {
	var wire []pullAnnotationWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if wire == nil {
		return nil, nil
	}
	out := make([]schema.PullAnnotation, 0, len(wire))
	for i := range wire {
		annotation, err := wire[i].annotation()
		if err != nil {
			return nil, err
		}
		out = append(out, annotation)
	}
	return out, nil
}

// decodePullListResponse reads one page of the pullable listing.
func decodePullListResponse(data []byte) (*schema.PullListResponse, error) {
	var wire pullListResponseWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	out := &schema.PullListResponse{Page: wire.Page, Limit: wire.Limit, Total: wire.Total}
	if wire.Transcripts != nil {
		out.Transcripts = make([]schema.PullTranscriptInfo, 0, len(wire.Transcripts))
	}
	for i := range wire.Transcripts {
		info, err := wire.Transcripts[i].info()
		if err != nil {
			return nil, err
		}
		out.Transcripts = append(out.Transcripts, info)
	}
	return out, nil
}
