package pull

import (
	"encoding/json"

	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// publicPersonColumns returns the account id and username the pull manifest and
// the local index keep for a person: a profile's, or two empty strings for a
// person the village shows this viewer anonymously. Nothing of an anonymous
// person is stored, so a hidden person's identity never reaches the disk.
func publicPersonColumns(person schema.VillagePublicOwner) (id, username string) {
	if !person.IsProfile() {
		return "", ""
	}
	return person.ID.String(), person.Username
}

// readerMeta returns a copy of meta whose owner is as this CLI may show them
// (see village.PublicOwnerForReader), with no project name beside an anonymous
// owner. The pipeline applies it to whatever its
// reader returned before it stores or re-encodes the metadata.
func readerMeta(meta *schema.PullTranscriptInfo) *schema.PullTranscriptInfo {
	out := *meta
	out.Owner = village.PublicOwnerForReader(meta.Owner)
	if !out.Owner.IsProfile() {
		// The repository identifies an anonymous owner; the village withholds
		// it, and this CLI keeps none that arrives anyway.
		out.ProjectName = nil
	}
	return &out
}

// projectNameColumn returns the project name the local index keeps: the
// served name, or an empty string when the village withheld or never recorded
// it.
func projectNameColumn(name *string) string {
	if name == nil {
		return ""
	}
	return *name
}

// readerAnnotations returns a copy of annotations whose authors are as this CLI
// may show them. Beside a transcript owner who is anonymous to this viewer,
// the repository-identifying targets (the project hash and the file path) are
// dropped, as the village withholds them, so none that arrives is kept.
func readerAnnotations(annotations []schema.PullAnnotation, transcriptOwner schema.VillagePublicOwner) []schema.PullAnnotation {
	if annotations == nil {
		return nil
	}
	anonymousOwner := !transcriptOwner.IsProfile()
	out := make([]schema.PullAnnotation, len(annotations))
	for i := range annotations {
		out[i] = annotations[i]
		out[i].Author = village.PublicOwnerForReader(annotations[i].Author)
		if anonymousOwner {
			out[i].TargetProjectHash = nil
			out[i].TargetFilePath = nil
		}
	}
	return out
}

// viewerAccount is the requester's village account as the public owner type
// names it, for the IsAccount "is this mine" check.
func (p *Pipeline) viewerAccount() schema.VillageUUID {
	return schema.VillageUUID(p.creds.UserID)
}

// storedOwnerChanged reports whether the owner a stored manifest names differs
// from the owner the village now shows: another kind, account or username.
func storedOwnerChanged(manifest PullManifest, owner schema.VillagePublicOwner) bool {
	id, username := publicPersonColumns(owner)
	return manifest.OwnerUserID != id || manifest.OwnerUsername != username
}

// storedHasProfileAuthor reports whether a stored manifest names any
// annotation author, which only a fresh annotation read can confirm is still
// shown.
func storedHasProfileAuthor(manifest PullManifest) bool {
	for _, a := range manifest.Annotations {
		if a.AuthorUserID != "" || a.AuthorUsername != "" {
			return true
		}
	}
	return false
}

// storedAuthorsChanged reports whether an annotation the manifest records is
// now authored, as the village shows it, by a different kind, account or
// username. Annotations present on only one side are left to the existing
// annotation currency rules.
func storedAuthorsChanged(manifest PullManifest, fresh []ManifestAnnotation) bool {
	current := make(map[string]ManifestAnnotation, len(fresh))
	for _, a := range fresh {
		current[a.ContentHash] = a
	}
	for _, stored := range manifest.Annotations {
		if now, ok := current[stored.ContentHash]; ok && now != stored {
			return true
		}
	}
	return false
}

// HeldIdentityCurrent reports whether a stored pull's manifest still shows the
// listed transcript's people as the village shows them, so a bulk pull may
// skip it without the pipeline. It is false when the manifest cannot be read,
// when the listed owner differs from the stored one, or when the manifest
// names an annotation author, because only a fresh annotation read (which the
// pipeline makes) can confirm that author is still shown.
func HeldIdentityCurrent(manifestBytes []byte, listed schema.PullTranscriptInfo) bool {
	var manifest PullManifest
	if json.Unmarshal(manifestBytes, &manifest) != nil {
		return false
	}
	return !storedOwnerChanged(manifest, village.PublicOwnerForReader(listed.Owner)) && !storedHasProfileAuthor(manifest)
}
