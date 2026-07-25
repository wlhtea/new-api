package seedance

import "encoding/json"

type rawJSONRequest struct {
	Model          json.RawMessage `json:"model"`
	Prompt         json.RawMessage `json:"prompt"`
	Duration       json.RawMessage `json:"duration"`
	Seconds        json.RawMessage `json:"seconds"`
	Size           json.RawMessage `json:"size"`
	Image          json.RawMessage `json:"image"`
	Images         json.RawMessage `json:"images"`
	InputReference json.RawMessage `json:"input_reference"`
	Metadata       json.RawMessage `json:"metadata"`
}

type rawMetadata struct {
	Duration           json.RawMessage `json:"duration"`
	Resolution         json.RawMessage `json:"resolution"`
	ImageBase64        json.RawMessage `json:"image_base64"`
	PromptOptimization json.RawMessage `json:"prompt_optimization"`
	MultiShot          json.RawMessage `json:"multi_shot"`
	StrictDuration     json.RawMessage `json:"strict_duration"`
	NegativePrompt     json.RawMessage `json:"negative_prompt"`
}

type requestInput struct {
	Raw      rawJSONRequest
	Metadata rawMetadata
}

type NormalizedRequest struct {
	Prompt             string
	ImageBase64        string
	Resolution         string
	Duration           int
	PromptOptimization *bool
	MultiShot          *bool
	StrictDuration     *bool
	NegativePrompt     string
}
