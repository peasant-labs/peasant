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

// PublicOwnerPullEnvelope is the first pull envelope that carries a person as
// a public owner. A row read under it or a later envelope must carry the
// public owner; the 0.2.0 account strings are refused rather than trusted as a
// profile.
const PublicOwnerPullEnvelope schema.PushContractVersion = "0.3.0"

// SetPullEnvelope records the pull envelope the village serves, for a client
// whose caller negotiated it on another client. NegotiatePull sets it itself.
func (c *VillageClient) SetPullEnvelope(envelope schema.PushContractVersion) {
	if envelope == "" {
		c.pullEnvelope.Store(nil)
		return
	}
	c.pullEnvelope.Store(&envelope)
}

// negotiatedPullEnvelope is the envelope this client decodes, or empty when it
// was never negotiated.
func (c *VillageClient) negotiatedPullEnvelope() schema.PushContractVersion {
	if envelope := c.pullEnvelope.Load(); envelope != nil {
		return *envelope
	}
	return ""
}

// resolvePullPerson selects the person from whichever envelope arrived. The
// 0.3.0 field wins when both are present, so a stray account string never
// overrides an anonymous value. A row carrying neither shape is refused.
//
// envelope is the negotiated pull envelope. Under PublicOwnerPullEnvelope or
// later only the public owner is read; a row with only the 0.2.0 account
// strings is refused. An empty envelope (a stored pull file, written by any
// earlier CLI) reads either shape.
func resolvePullPerson(envelope schema.PushContractVersion, current json.RawMessage, legacyID, legacyUsername *string, currentField, legacyField string) (schema.VillagePublicOwner, error) {
	if current == nil && legacyID != nil && envelope != "" && compareSemver(envelope, PublicOwnerPullEnvelope) >= 0 {
		return schema.VillagePublicOwner{}, fmt.Errorf("read pull envelope: pull contract %s carries %s, but the row has only the %s account strings of pull contract 0.2.0; refusing to read an account string as a profile", envelope, currentField, legacyField)
	}
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

func (w *pullTranscriptInfoWire) info(envelope schema.PushContractVersion) (schema.PullTranscriptInfo, error) {
	owner, err := resolvePullPerson(envelope, w.Owner, w.OwnerUserID, w.OwnerUsername, "owner", "ownerUserId")
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

func (w *pullAnnotationWire) annotation(envelope schema.PushContractVersion) (schema.PullAnnotation, error) {
	author, err := resolvePullPerson(envelope, w.Author, w.AuthorUserID, w.AuthorUsername, "author", "authorUserId")
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

// DecodePullTranscriptInfo reads one stored pull metadata snapshot, written
// by this or any earlier CLI, in either supported envelope.
func DecodePullTranscriptInfo(data []byte) (schema.PullTranscriptInfo, error) {
	return decodePullTranscriptInfo(data, "")
}

func decodePullTranscriptInfo(data []byte, envelope schema.PushContractVersion) (schema.PullTranscriptInfo, error) {
	var wire pullTranscriptInfoWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return schema.PullTranscriptInfo{}, err
	}
	return wire.info(envelope)
}

// DecodePullAnnotation reads one stored pulled annotation payload, written by
// this or any earlier CLI, in either supported envelope.
func DecodePullAnnotation(data []byte) (schema.PullAnnotation, error) {
	var wire pullAnnotationWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return schema.PullAnnotation{}, err
	}
	return wire.annotation("")
}

// decodePullAnnotations reads the annotations endpoint's bare array.
func decodePullAnnotations(data []byte, envelope schema.PushContractVersion) ([]schema.PullAnnotation, error) {
	var wire []pullAnnotationWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	if wire == nil {
		return nil, nil
	}
	out := make([]schema.PullAnnotation, 0, len(wire))
	for i := range wire {
		annotation, err := wire[i].annotation(envelope)
		if err != nil {
			return nil, err
		}
		out = append(out, annotation)
	}
	return out, nil
}

// decodePullListResponse reads one page of the pullable listing.
func decodePullListResponse(data []byte, envelope schema.PushContractVersion) (*schema.PullListResponse, error) {
	var wire pullListResponseWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	out := &schema.PullListResponse{Page: wire.Page, Limit: wire.Limit, Total: wire.Total}
	if wire.Transcripts != nil {
		out.Transcripts = make([]schema.PullTranscriptInfo, 0, len(wire.Transcripts))
	}
	for i := range wire.Transcripts {
		info, err := wire.Transcripts[i].info(envelope)
		if err != nil {
			return nil, err
		}
		out.Transcripts = append(out.Transcripts, info)
	}
	return out, nil
}
