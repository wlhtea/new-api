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

type generateRequest struct {
	Prompt             string `json:"prompt"`
	ImageBase64        string `json:"image_base64,omitempty"`
	Duration           int    `json:"duration"`
	Resolution         string `json:"resolution"`
	PromptOptimization *bool  `json:"prompt_optimization,omitempty"`
	MultiShot          *bool  `json:"multi_shot,omitempty"`
	StrictDuration     *bool  `json:"strict_duration,omitempty"`
	NegativePrompt     string `json:"negative_prompt,omitempty"`
}

type providerEnvelope struct {
	RequestID  string          `json:"requestId,omitempty"`
	TaskID     string          `json:"task_id,omitempty"`
	Status     string          `json:"status,omitempty"`
	Success    *bool           `json:"success,omitempty"`
	ErrCode    json.RawMessage `json:"errCode,omitempty"`
	ErrMessage string          `json:"errMessage,omitempty"`
	Message    string          `json:"message,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

type cleanedTaskData struct {
	RequestID  string `json:"requestId,omitempty"`
	Success    *bool  `json:"success,omitempty"`
	ErrCode    string `json:"errCode,omitempty"`
	ErrMessage string `json:"errMessage,omitempty"`
	Status     string `json:"status,omitempty"`
	Message    string `json:"message,omitempty"`
	Model      string `json:"model,omitempty"`
	Seconds    string `json:"seconds,omitempty"`
	Size       string `json:"size,omitempty"`
}
