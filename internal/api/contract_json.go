package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// decodeStrict decodes a JSON body into a contract type, refusing an unknown
// field, a missing body, and anything after the one value.
func decodeStrict(body io.Reader, into any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("the body holds more than one JSON value")
	}
	return nil
}

// writeContract answers 200 with a contract value, after checking it the way
// a client does. A value that breaks the contract is not sent.
func writeContract(w http.ResponseWriter, value contractValue) {
	if err := value.Validate(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "The answer breaks the Local API contract, so it is not sent: "+err.Error()+". Retry; if it repeats, report it.", publishingContractBreachCode)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

// contractValue is a contract type that checks its own invariants.
type contractValue interface{ Validate() error }
