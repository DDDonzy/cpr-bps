package kernel

import (
	"errors"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type closedLoader struct{}

func (closedLoader) Load(string) (any, error) {
	return nil, errors.New("external schema loading is disabled")
}

func compileSchema(schema any) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.UseLoader(closedLoader{})
	c.AssertFormat()
	const location = "https://cpr.invalid/tool.schema.json"
	if err := c.AddResource(location, schema); err != nil {
		return nil, err
	}
	return c.Compile(location)
}
func validateSchemaValue(schema, value any) error {
	compiled, err := compileSchema(schema)
	if err != nil {
		return err
	}
	return compiled.Validate(value)
}
func declaredSchema(spec map[string]any) (any, bool) {
	for _, key := range []string{"parameters", "inputSchema", "input_schema"} {
		if value, ok := spec[key]; ok {
			return value, true
		}
	}
	return nil, false
}
func argumentsMatch(value any, spec map[string]any) bool {
	schema, exists := declaredSchema(spec)
	return !exists || validateSchemaValue(schema, value) == nil
}
