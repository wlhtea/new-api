package controller

// Channel-level Volcengine Ark Assets (素材库) relay.
//
// Unlike the system-level outbound asset implementation on feat/doubao-asset,
// this controller relays through a regular channel of type ChannelTypeVolcAssets:
//   - channel key:      "AccessKey|SecretKey" (multi-key supported, one per line)
//   - channel base_url: upstream gateway, default https://open.volcengineapi.com
//     (a path suffix is allowed, e.g. https://gateway.example.com/seedance-gateway)
//   - channel other info (JSON): {"project_name": "...", "region": "cn-beijing"}
//
// Downstream entry: POST /v1/assets/{Action} with a JSON body that carries a
// pseudo "model" field (used only for channel selection, e.g. "seedance-assets").
// The request body is forwarded verbatim to the upstream Assets API; when
// ProjectName is empty it is injected from the channel's other info.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

const (
	volcAssetService    = "ark"
	volcAssetAPIVersion = "2024-01-01"
	volcAssetDefaultURL = "https://open.volcengineapi.com"
	volcAssetDefaultRegion = "cn-beijing"
)

// Whitelisted upstream Actions for the Assets API.
var volcAssetAllowedActions = map[string]bool{
	"CreateAssetGroup": true,
	"ListAssetGroups":  true,
	"GetAssetGroup":    true,
	"UpdateAssetGroup": true,
	"DeleteAssetGroup": true,
	"CreateAsset":      true,
	"GetAsset":         true,
	"ListAssets":       true,
	"UpdateAsset":      true,
	"DeleteAsset":      true,
}

func RelayVolcAsset(c *gin.Context) {
	action := c.Param("action")
	if !volcAssetAllowedActions[action] {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("unsupported asset action: %s", action),
			"type":    "invalid_request_error",
		}})
		return
	}

	channelId, ok := common.GetContextKey(c, constant.ContextKeyChannelId)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "channel not selected", "type": "server_error"}})
		return
	}
	channel, err := model.CacheGetChannel(channelId.(int))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "channel not found: " + err.Error(), "type": "server_error"}})
		return
	}
	if channel.Type != constant.ChannelTypeVolcAssets {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
			"message": fmt.Sprintf("channel %d is not a VolcAssets channel (type=%d)", channel.Id, channel.Type),
			"type":    "invalid_request_error",
		}})
		return
	}

	key, _, keyErr := channel.GetNextEnabledKey()
	if keyErr != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "no enabled key on channel: " + keyErr.Error(), "type": "server_error"}})
		return
	}
	parts := strings.SplitN(strings.TrimSpace(key), "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{
			"message": "channel key must be in \"AccessKey|SecretKey\" format",
			"type":    "server_error",
		}})
		return
	}
	ak, sk := parts[0], parts[1]

	baseURL := strings.TrimSuffix(strings.TrimSpace(channel.GetBaseURL()), "/")
	if baseURL == "" {
		baseURL = volcAssetDefaultURL
	}
	otherInfo := channel.GetOtherInfo()
	region := volcAssetDefaultRegion
	if r, ok := otherInfo["region"].(string); ok && r != "" {
		region = r
	}
	defaultProject := ""
	if p, ok := otherInfo["project_name"].(string); ok {
		defaultProject = strings.TrimSpace(p)
	}

	// Read body via reusable storage (distributor may have consumed Request.Body).
	bodyStorage, err := common.GetBodyStorage(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "read body failed: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	bodyBytes, err := bodyStorage.Bytes()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "read body failed", "type": "invalid_request_error"}})
		return
	}
	// strip the pseudo model field before forwarding
	var bodyMap map[string]interface{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &bodyMap); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "body must be a JSON object", "type": "invalid_request_error"}})
			return
		}
	} else {
		bodyMap = map[string]interface{}{}
	}
	delete(bodyMap, "model")
	if defaultProject != "" {
		if pn, exists := bodyMap["ProjectName"]; !exists || pn == "" {
			bodyMap["ProjectName"] = defaultProject
		}
	}
	forwardBody, err := json.Marshal(bodyMap)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "marshal body failed", "type": "invalid_request_error"}})
		return
	}

	upstreamURL := fmt.Sprintf("%s?Action=%s&Version=%s", baseURL, url.QueryEscape(action), volcAssetAPIVersion)
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(forwardBody))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "build upstream request failed: " + err.Error(), "type": "server_error"}})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	signVolcAssetRequest(req, forwardBody, ak, sk, region)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream request failed: " + err.Error(), "type": "server_error"}})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "read upstream response failed", "type": "server_error"}})
		return
	}
	c.Data(resp.StatusCode, "application/json; charset=utf-8", respBody)
}

// testVolcAssetsChannel probes a VolcAssets channel by calling
// ListAssetGroups (PageSize=1) with the channel's AK/SK.
func testVolcAssetsChannel(ctx context.Context, channel *model.Channel) error {
	key, _, keyErr := channel.GetNextEnabledKey()
	if keyErr != nil {
		return fmt.Errorf("no enabled key: %v", keyErr)
	}
	parts := strings.SplitN(strings.TrimSpace(key), "|", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("channel key must be in \"AccessKey|SecretKey\" format")
	}
	baseURL := strings.TrimSuffix(strings.TrimSpace(channel.GetBaseURL()), "/")
	if baseURL == "" {
		baseURL = volcAssetDefaultURL
	}
	region := volcAssetDefaultRegion
	if r, ok := channel.GetOtherInfo()["region"].(string); ok && r != "" {
		region = r
	}
	bodyMap := map[string]interface{}{"PageSize": 1}
	if p, ok := channel.GetOtherInfo()["project_name"].(string); ok && strings.TrimSpace(p) != "" {
		bodyMap["ProjectName"] = strings.TrimSpace(p)
	}
	bodyBytes, _ := json.Marshal(bodyMap)
	upstreamURL := fmt.Sprintf("%s?Action=ListAssetGroups&Version=%s", baseURL, volcAssetAPIVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	signVolcAssetRequest(req, bodyBytes, parts[0], parts[1], region)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, string(respBody))
	}
	var parsed struct {
		ResponseMetadata struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
	}
	if err := json.Unmarshal(respBody, &parsed); err == nil && parsed.ResponseMetadata.Error != nil {
		return fmt.Errorf("upstream error %s: %s", parsed.ResponseMetadata.Error.Code, parsed.ResponseMetadata.Error.Message)
	}
	return nil
}

// signVolcAssetRequest signs req with Volcengine HMAC-SHA256 (ark service).
func signVolcAssetRequest(req *http.Request, bodyBytes []byte, accessKey, secretKey, region string) {
	hexPayloadHash := hex.EncodeToString(common.Sha256Raw(bodyBytes))

	t := time.Now().UTC()
	xDate := t.Format("20060102T150405Z")
	shortDate := t.Format("20060102")

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Date", xDate)
	req.Header.Set("X-Content-Sha256", hexPayloadHash)

	// Canonical query string
	queryParams := req.URL.Query()
	sortedKeys := make([]string, 0, len(queryParams))
	for k := range queryParams {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	var queryParts []string
	for _, k := range sortedKeys {
		values := queryParams[k]
		sort.Strings(values)
		for _, v := range values {
			queryParts = append(queryParts, fmt.Sprintf("%s=%s", url.QueryEscape(k), url.QueryEscape(v)))
		}
	}
	canonicalQueryString := strings.Join(queryParts, "&")

	// Canonical headers
	headersToSign := map[string]string{
		"host":             req.URL.Host,
		"x-date":           xDate,
		"x-content-sha256": hexPayloadHash,
	}
	if req.Header.Get("Content-Type") != "" {
		headersToSign["content-type"] = req.Header.Get("Content-Type")
	}
	var signedHeaderKeys []string
	for k := range headersToSign {
		signedHeaderKeys = append(signedHeaderKeys, k)
	}
	sort.Strings(signedHeaderKeys)
	var canonicalHeaders strings.Builder
	for _, k := range signedHeaderKeys {
		canonicalHeaders.WriteString(k)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(strings.TrimSpace(headersToSign[k]))
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(signedHeaderKeys, ";")

	canonicalRequest := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s",
		req.Method,
		req.URL.Path,
		canonicalQueryString,
		canonicalHeaders.String(),
		signedHeaders,
		hexPayloadHash,
	)

	hexHashedCanonicalRequest := hex.EncodeToString(common.Sha256Raw([]byte(canonicalRequest)))
	credentialScope := fmt.Sprintf("%s/%s/%s/request", shortDate, region, volcAssetService)
	stringToSign := fmt.Sprintf("HMAC-SHA256\n%s\n%s\n%s", xDate, credentialScope, hexHashedCanonicalRequest)

	kDate := common.HmacSha256Raw([]byte(shortDate), []byte(secretKey))
	kRegion := common.HmacSha256Raw([]byte(region), kDate)
	kService := common.HmacSha256Raw([]byte(volcAssetService), kRegion)
	kSigning := common.HmacSha256Raw([]byte("request"), kService)
	signature := hex.EncodeToString(common.HmacSha256Raw([]byte(stringToSign), kSigning))

	req.Header.Set("Authorization", fmt.Sprintf("HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, credentialScope, signedHeaders, signature))
}
