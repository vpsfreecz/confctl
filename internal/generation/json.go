package generation

// DecodeJSONValue decodes one complete JSON value into the existing ordered
// model. It performs JSON admission only, not generation-record validation.
// Objects are Object, arrays []any, numbers json.Number, with string/bool/nil
// scalars. It retains the parser's documented finite provider-admission limits.
func DecodeJSONValue(source []byte) (any, error) {
	return decodeRecord(source)
}
