package kickstart_test

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// decodeStrictFixture decodes exactly one typed YAML document. All kickstart
// identity fixtures use this boundary so a misspelled path or multiplicity key
// fails instead of silently erasing the evidence the test meant to exercise.
func decodeStrictFixture(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("found a second YAML document")
		}
		return err
	}
	return nil
}
