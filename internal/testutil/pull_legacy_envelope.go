package testutil

import (
	"encoding/json"
	"fmt"
)

// LegacyPullEnvelope re-encodes a current pull response (a PullTranscriptInfo,
// a PullListResponse, or a PullAnnotation slice) into pull contract 0.2.0, the
// envelope a village that has not adopted the public owner serves: each
// "owner" becomes ownerUserId/ownerUsername and each "author" becomes
// authorUserId/authorUsername. A 0.2.0 village showed every person, so only a
// profile can be re-encoded; an anonymous person has no 0.2.0 form.
func LegacyPullEnvelope(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, err
	}
	if err := legacyPullPeople(tree); err != nil {
		return nil, err
	}
	return json.Marshal(tree)
}

func legacyPullPeople(node any) error {
	switch typed := node.(type) {
	case []any:
		for _, item := range typed {
			if err := legacyPullPeople(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for field, prefix := range map[string]string{"owner": "owner", "author": "author"} {
			person, ok := typed[field].(map[string]any)
			if !ok {
				continue
			}
			if person["kind"] != "profile" {
				return fmt.Errorf("pull contract 0.2.0 has no form for a %v %s", person["kind"], field)
			}
			delete(typed, field)
			typed[prefix+"UserId"] = person["id"]
			typed[prefix+"Username"] = person["username"]
		}
		if transcripts, ok := typed["transcripts"]; ok {
			return legacyPullPeople(transcripts)
		}
	}
	return nil
}
