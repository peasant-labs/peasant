package pull

import (
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
// (see village.PublicOwnerForReader). The pipeline applies it to whatever its
// reader returned before it stores or re-encodes the metadata.
func readerMeta(meta *schema.PullTranscriptInfo) *schema.PullTranscriptInfo {
	out := *meta
	out.Owner = village.PublicOwnerForReader(meta.Owner)
	return &out
}

// readerAnnotations returns a copy of annotations whose authors are as this CLI
// may show them.
func readerAnnotations(annotations []schema.PullAnnotation) []schema.PullAnnotation {
	if annotations == nil {
		return nil
	}
	out := make([]schema.PullAnnotation, len(annotations))
	for i := range annotations {
		out[i] = annotations[i]
		out[i].Author = village.PublicOwnerForReader(annotations[i].Author)
	}
	return out
}

// viewerAccount is the requester's village account as the public owner type
// names it, for the IsAccount "is this mine" check.
func (p *Pipeline) viewerAccount() schema.VillageUUID {
	return schema.VillageUUID(p.creds.UserID)
}
