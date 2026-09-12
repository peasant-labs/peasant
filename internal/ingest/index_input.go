package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
)

// indexInputDigest identifies the bytes and context consumed by a parser. It
// excludes producer/format versions, artifact metadata and preview mode. A caller
// must first establish that input was captured; absent input has no digest.
func indexInputDigest(session DiscoveredSession, transcript []byte, tree *openCodeJSONInput) string {
	digest := sha256.New()
	writeIndexInputField(digest, []byte("peasant.index-input.v1"))
	writeIndexInputField(digest, []byte(session.SessionID))
	writeIndexInputField(digest, []byte(session.Harness))
	if session.Harness == HarnessOpenCode {
		writeIndexInputField(digest, []byte(strconv.Itoa(int(session.TranscriptOrigin))))
	}
	if tree == nil {
		writeIndexInputField(digest, []byte("file"))
		writeIndexInputField(digest, transcript)
	} else {
		writeIndexInputField(digest, []byte("opencode-json-tree"))
		writeIndexInputField(digest, []byte(strconv.Itoa(len(tree.Messages))))
		for _, message := range tree.Messages {
			writeIndexInputField(digest, []byte(message.File.Name))
			writeIndexInputField(digest, message.File.Data)
			writeIndexInputField(digest, []byte(strconv.FormatBool(message.PartsMissing)))
			writeIndexInputField(digest, []byte(strconv.Itoa(len(message.Parts))))
			for _, part := range message.Parts {
				writeIndexInputField(digest, []byte(part.Name))
				writeIndexInputField(digest, part.Data)
			}
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func writeIndexInputField(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}

// openCodeJSONInput owns the exact native records read for one strict index run.
// An empty Messages slice is a captured empty directory, not an absent source.
type openCodeJSONInput struct {
	Messages []openCodeJSONMessage
}

type openCodeJSONMessage struct {
	File         openCodeJSONFile
	PartsMissing bool
	Parts        []openCodeJSONFile
}

type openCodeJSONFile struct {
	Name string
	Data []byte
}
