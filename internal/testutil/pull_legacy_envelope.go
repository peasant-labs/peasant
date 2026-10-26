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

// LegacyCollectivePeople re-encodes a current collective answer (records,
// a detail response, or linked repositories) the way a village before Village
// API 0.29.0 sent it: creator becomes a created_by account id and linked_by
// becomes the linker's account id string.
func LegacyCollectivePeople(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(body, &tree); err != nil {
		return nil, err
	}
	legacyCollectiveNode(tree)
	return json.Marshal(tree)
}

func legacyCollectiveNode(node any) {
	switch typed := node.(type) {
	case []any:
		for _, item := range typed {
			legacyCollectiveNode(item)
		}
	case map[string]any:
		if person, ok := typed["creator"].(map[string]any); ok {
			delete(typed, "creator")
			typed["created_by"] = legacyAccountID(person)
		}
		if person, ok := typed["linked_by"].(map[string]any); ok {
			typed["linked_by"] = legacyAccountID(person)
		}
		for _, value := range typed {
			legacyCollectiveNode(value)
		}
	}
}

// legacyAccountID is the account id an older village sent for a person. An
// anonymous person had no such field then, so a fixed account id stands in.
func legacyAccountID(person map[string]any) any {
	if id, ok := person["id"].(string); ok {
		return id
	}
	return "00000000-0000-4000-8000-0000000000ff"
}
