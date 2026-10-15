package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Preserve arbitrary JSON integer and decimal tokens at public service boundaries.
// Like json.Unmarshal, require one complete value rather than accepting a suffix.
func decodeJSONNumbers(b []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(value); err != nil {
		return err
	}
	var trailing json.RawMessage
	err := d.Decode(&trailing)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("extra JSON content")
	}
	return err
}
