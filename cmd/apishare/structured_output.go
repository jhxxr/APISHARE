package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Shared by attempts as immutable data. The selected upstream controls only
// the wire representation, never the client's original schema.
type canonOutputFormat struct {
	Schema map[string]any
	check  *jsonschema.Schema
}

type noSchemaResourceLoader struct{}

func (noSchemaResourceLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema resources are not supported")
}

func validStructuredOutputMode(mode string) bool {
	return mode == "" || mode == "json_schema" || mode == "json_object" || mode == "disabled"
}

func parseAnthropicOutputFormat(body map[string]any) (*canonOutputFormat, error) {
	raw := object(body["output_config"])["format"]
	if legacy := body["output_format"]; legacy != nil {
		if raw != nil && jsonText(raw) != jsonText(legacy) {
			return nil, fmt.Errorf("output_format conflicts with output_config.format")
		}
		raw = legacy
	}
	if raw == nil {
		return nil, nil
	}
	format := object(raw)
	if str(format["type"]) != "json_schema" {
		return nil, unsupported("Anthropic output format other than json_schema")
	}
	schema := object(format["schema"])
	if schema == nil {
		return nil, fmt.Errorf("structured output schema must be a JSON Schema object")
	}
	if len(jsonText(schema)) > 1<<20 {
		return nil, fmt.Errorf("structured output schema exceeds 1 MiB")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	// Local $ref/$defs work. Client schemas must never read files or fetch URLs.
	compiler.UseLoader(noSchemaResourceLoader{})
	const resource = "https://apishare.invalid/structured-output.json"
	if err := compiler.AddResource(resource, schema); err != nil {
		return nil, fmt.Errorf("invalid structured output schema")
	}
	check, err := compiler.Compile(resource)
	if err != nil {
		// Compiler errors can include schema contents; keep logs free of them.
		return nil, fmt.Errorf("invalid structured output schema or unsupported external reference")
	}
	return &canonOutputFormat{Schema: schema, check: check}, nil
}

func prepareStructuredOutput(format clientFormat, cr *canonReq, mode string) (*canonReq, error) {
	if cr.OutputFormat == nil {
		return cr, nil
	}
	if !validStructuredOutputMode(mode) || mode == "disabled" {
		return nil, fmt.Errorf("upstream does not support converted structured output; choose a capable upstream or enable JSON compatibility")
	}
	if mode != "json_object" {
		return cr, nil
	}
	if format != FormatOpenAIChat && format != FormatOpenAIResponse {
		return nil, fmt.Errorf("JSON compatibility requires a Chat Completions or Responses upstream")
	}
	if str(cr.OutputFormat.Schema["type"]) != "object" {
		return nil, fmt.Errorf("JSON compatibility requires a schema with root type object; use a JSON Schema capable upstream for other roots")
	}
	copy := *cr
	copy.System = joinSystem(cr.System, "Return the final answer as a JSON object matching the following JSON Schema. Do not wrap it in Markdown or add explanatory text. This requirement applies to the final answer, not to tool calls. JSON Schema:\n"+jsonText(cr.OutputFormat.Schema))
	return &copy, nil
}

func renderStructuredOutput(format clientFormat, cr *canonReq, mode string, body map[string]any) error {
	if cr.OutputFormat == nil {
		return nil
	}
	schema := cr.OutputFormat.Schema
	switch format {
	case FormatOpenAIChat:
		if mode == "json_object" {
			body["response_format"] = map[string]any{"type": "json_object"}
		} else {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "structured_output", "strict": true, "schema": schema}}
		}
	case FormatOpenAIResponse:
		output := map[string]any{"type": "json_schema", "name": "structured_output", "strict": true, "schema": schema}
		if mode == "json_object" {
			output = map[string]any{"type": "json_object"}
		}
		body["text"] = map[string]any{"format": output}
	case FormatAnthropic:
		config := object(body["output_config"])
		if config == nil {
			config = map[string]any{}
			body["output_config"] = config
		}
		config["format"] = map[string]any{"type": "json_schema", "schema": schema}
	case FormatGemini:
		config := object(body["generationConfig"])
		if config == nil {
			config = map[string]any{}
			body["generationConfig"] = config
		}
		config["responseMimeType"] = "application/json"
		config["responseJsonSchema"] = schema
	default:
		return unsupported("structured output on this upstream protocol")
	}
	return nil
}

// Validate before committing converted content, including when a provider
// accepts but ignores response_format. Thinking and tool arguments are not the
// final JSON output. Preserve explicit refusals and truncation stop reasons.
func validateStructuredOutput(spec *canonOutputFormat, response *canonResponse) error {
	if spec == nil || response.hasTools() || response.Finish == "content_filter" || response.Finish == "length" {
		return nil
	}
	var text strings.Builder
	for _, block := range response.Blocks {
		if block.Kind == "text" {
			text.WriteString(block.Text)
		}
	}
	content := text.String()
	if !json.Valid([]byte(content)) {
		return fmt.Errorf("upstream structured output is not valid JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("upstream structured output is not valid JSON")
	}
	if err := spec.check.Validate(value); err != nil {
		return fmt.Errorf("upstream structured output does not match the requested JSON Schema")
	}
	return nil
}
