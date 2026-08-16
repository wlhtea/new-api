package opencodego

const ChannelName = "OpenCode Go"

type AdvertisedModelProtocol struct {
	Model    string
	Protocol Protocol
}

var advertisedModelProtocolManifest = []AdvertisedModelProtocol{
	{Model: "grok-4.5", Protocol: ProtocolChat},
	{Model: "glm-5.2", Protocol: ProtocolChat},
	{Model: "glm-5.1", Protocol: ProtocolChat},
	{Model: "kimi-k3", Protocol: ProtocolChat},
	{Model: "kimi-k2.7-code", Protocol: ProtocolChat},
	{Model: "kimi-k2.6", Protocol: ProtocolChat},
	{Model: "deepseek-v4-pro", Protocol: ProtocolChat},
	{Model: "deepseek-v4-flash", Protocol: ProtocolChat},
	{Model: "mimo-v2.5", Protocol: ProtocolChat},
	{Model: "mimo-v2.5-pro", Protocol: ProtocolChat},
	{Model: "hy3", Protocol: ProtocolChat},
	{Model: "minimax-m3", Protocol: ProtocolMessages},
	{Model: "minimax-m2.7", Protocol: ProtocolMessages},
	{Model: "minimax-m2.5", Protocol: ProtocolMessages},
	{Model: "qwen3.8-max", Protocol: ProtocolMessages},
	{Model: "qwen3.7-max", Protocol: ProtocolMessages},
	{Model: "qwen3.7-plus", Protocol: ProtocolMessages},
	{Model: "qwen3.6-plus", Protocol: ProtocolMessages},
	{Model: "gpt-5.6-luna", Protocol: ProtocolResponses},
}

var ModelList = buildAdvertisedModelList(advertisedModelProtocolManifest)

var builtInModelProtocols = buildBuiltInModelProtocols(advertisedModelProtocolManifest)

func AdvertisedModelProtocolManifest() []AdvertisedModelProtocol {
	return append([]AdvertisedModelProtocol(nil), advertisedModelProtocolManifest...)
}

func buildAdvertisedModelList(manifest []AdvertisedModelProtocol) []string {
	models := make([]string, len(manifest))
	for index, entry := range manifest {
		models[index] = entry.Model
	}
	return models
}

func buildBuiltInModelProtocols(manifest []AdvertisedModelProtocol) map[string]Protocol {
	protocols := make(map[string]Protocol, len(manifest))
	for _, entry := range manifest {
		protocols[entry.Model] = entry.Protocol
	}
	return protocols
}
