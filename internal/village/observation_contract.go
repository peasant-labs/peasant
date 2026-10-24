package village

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/peasant-labs/schema"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v5"
)

var contributionDiscoveryContract = sync.OnceValues(func() (*jsonschema.Schema, error) {
	compiler := schema.NewJSONSchemaCompiler()
	const document = "https://schema.invalid/village-observation.json"
	if err := compiler.AddResource(document, bytes.NewReader(schema.VillageAPISpecJSON())); err != nil {
		return nil, err
	}
	return compiler.Compile(document + "#/components/schemas/SchemaVillageContributedCollectivesResponse")
})

// decodeContributionDiscovery validates raw requiredness against the SAME
// embedded canonical contract Village serves, before Go can coerce absent/null
// counters into zero. It defines no private wire shape or required-field list.
func decodeContributionDiscovery(raw json.RawMessage) (schema.VillageContributedCollectivesResponse, error) {
	var response schema.VillageContributedCollectivesResponse
	contract, err := contributionDiscoveryContract()
	if err != nil {
		return response, fmt.Errorf("compile canonical contribution discovery validator: %w; no relevance can be installed; repair the pinned contract artifact", err)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return response, err
	}
	if err := contract.Validate(value); err != nil {
		return response, fmt.Errorf("validate complete contribution discovery: required canonical fields or counter types are invalid; no relevance was replaced; correct the peasant online response")
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return response, err
	}
	return response, nil
}
