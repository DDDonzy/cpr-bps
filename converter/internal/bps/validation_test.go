package bps

import (
	"strings"
	"testing"
)

func TestValidationSummaryDoesNotExposeInputOrArbitraryNames(t *testing.T) {
	raw := `{"detail":[{"type":"extra_forbidden","loc":["body","max_output_tokens"],"msg":"SECRET_MESSAGE","input":"Bearer SECRET_TOKEN"},{"type":"SECRET_TYPE","loc":["body","SECRET_FIELD"],"ctx":{"pattern":"SECRET_PATTERN"}}]}`
	value := validationSummary(strings.NewReader(raw))
	if strings.Contains(value, "SECRET") || !strings.Contains(value, "max_output_tokens") || !strings.Contains(value, "extra_forbidden") {
		t.Fatal("unsafe or missing validation summary")
	}
}

func TestCommonValidationFormatsRemainSanitized(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"error":{"param":"max_output_tokens","code":"unsupported_parameter","message":"SECRET_MESSAGE","input":"Bearer SECRET_TOKEN"}}`, "unsupported_parameter"},
		{`{"detail":"Unsupported parameter: max_output_tokens; SECRET_TOKEN"}`, "unsupported_parameter"},
		{`Unrecognized request argument supplied: max_output_tokens SECRET_TOKEN`, "unsupported_parameter"},
		{`{"error":{"param":"max_output_tokens","message":"Unsupported value for max_output_tokens: SECRET_VALUE"}}`, "unsupported_value"},
	}
	for _, c := range cases {
		v := validationSummary(strings.NewReader(c.raw))
		if !strings.Contains(v, "max_output_tokens") || !strings.Contains(v, c.want) || strings.Contains(v, "SECRET") || strings.Contains(v, "Bearer") {
			t.Fatal("missing or unsafe common validation summary")
		}
	}
	if validationSummary(strings.NewReader(`{"error":{"param":"SECRET_PARAM","message":"SECRET_MESSAGE"}}`)) != "" {
		t.Fatal("arbitrary error leaked")
	}
}
