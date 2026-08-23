package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// decodeStrictServiceJSON accepts exactly one JSON value. Service definitions,
// bindings, and export hand-offs are schema-bearing protocol documents, so a
// valid prefix must never hide a second value or arbitrary trailing bytes.
func decodeStrictServiceJSON(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return fmt.Errorf("invalid trailing JSON data: %w", err)
	}
	return nil
}
