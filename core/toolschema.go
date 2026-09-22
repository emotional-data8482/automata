package core

import "errors"

// validateSchema checks a schema map against the supported subset and
// rejects unsupported assertion keywords explicitly. Provider-specific
// annotation keywords are preserved verbatim for adapters; core does not
// claim to enforce an entire JSON Schema dialect. The enforced subset and
// the rejection rules live in schemacontract.go.
func validateSchema(s map[string]any) error {
	_, err := compileSchemaContract(s)
	return err
}

// validateSchemaValue checks one decoded JSON value against a schema map.
// The value must be decoded with json.Number preservation (UseNumber).
func validateSchemaValue(s map[string]any, v any, path string) error {
	contract, err := compileSchemaContract(s)
	if err != nil {
		return err
	}
	if violations := contract.validate(v, path); len(violations) > 0 {
		return errors.New(violations[0])
	}
	return nil
}
