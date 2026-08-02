package service

import (
	"errors"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"golang.org/x/net/html"
)

const (
	OpenCodeGoSSRParserVersion = "ssr-v1"

	openCodeGoMaxSSRDocumentSize = 4 * 1024 * 1024
	openCodeGoMaxSSRTokens       = 300_000
	openCodeGoMaxResetSeconds    = int64(400 * 24 * 60 * 60)
)

var openCodeGoWorkspaceIDPattern = regexp.MustCompile(`(?i)^wrk_[a-z0-9]+$`)

type OpenCodeGoDiscoveredWorkspace struct {
	ID   string
	Name string
}

type OpenCodeGoAuthoritativeQuotaWindow struct {
	Kind         string
	UsedPercent  float64
	ResetSeconds int64
	ResetAt      int64
	FetchedAt    int64
}

type OpenCodeGoAuthoritativeQuotaSnapshot struct {
	Windows       []OpenCodeGoAuthoritativeQuotaWindow
	FetchedAt     int64
	NextRefreshAt int64
}

type OpenCodeGoConsolePage struct {
	WorkspaceID              string
	WorkspaceName            string
	Email                    string
	Workspaces               []OpenCodeGoDiscoveredWorkspace
	MembershipStatus         string
	SubscriptionReference    string
	Quota                    *OpenCodeGoAuthoritativeQuotaSnapshot
	QuotaParseError          string
	ReferralCode             string
	AvailableReferralRewards int
	UsedReferralRewards      int
	ChinaModelsEnabled       *bool
}

type openCodeGoSSRTokenKind uint8

const (
	openCodeGoSSRTokenIdentifier openCodeGoSSRTokenKind = iota + 1
	openCodeGoSSRTokenString
	openCodeGoSSRTokenNumber
	openCodeGoSSRTokenPunctuation
)

type openCodeGoSSRToken struct {
	kind  openCodeGoSSRTokenKind
	value string
}

func ParseOpenCodeGoConsolePage(document string, currentWorkspaceID string, fetchedAt time.Time) (*OpenCodeGoConsolePage, error) {
	if len(document) == 0 {
		return nil, errors.New("OpenCode Go console page is empty")
	}
	if len(document) > openCodeGoMaxSSRDocumentSize {
		return nil, errors.New("OpenCode Go console page is too large")
	}
	if !openCodeGoWorkspaceIDPattern.MatchString(currentWorkspaceID) {
		return nil, errors.New("OpenCode Go console workspace ID is invalid")
	}

	doc, err := html.Parse(strings.NewReader(document))
	if err != nil {
		return nil, errors.New("OpenCode Go console HTML could not be parsed")
	}
	scriptSource, visibleText, chinaModelsEnabled := extractOpenCodeGoConsoleHTML(doc)
	tokens := lexOpenCodeGoSSR(scriptSource)
	if len(tokens) > openCodeGoMaxSSRTokens {
		return nil, errors.New("OpenCode Go console hydration payload is too large")
	}

	page := &OpenCodeGoConsolePage{
		WorkspaceID:        currentWorkspaceID,
		MembershipStatus:   model.OpenCodeGoMembershipUnknown,
		ChinaModelsEnabled: chinaModelsEnabled,
	}
	page.Workspaces = extractOpenCodeGoWorkspaces(tokens)
	for _, workspace := range page.Workspaces {
		if strings.EqualFold(workspace.ID, currentWorkspaceID) {
			page.WorkspaceName = workspace.Name
			break
		}
	}
	if !containsOpenCodeGoWorkspace(page.Workspaces, currentWorkspaceID) {
		page.Workspaces = append(page.Workspaces, OpenCodeGoDiscoveredWorkspace{ID: currentWorkspaceID})
		sort.Slice(page.Workspaces, func(i, j int) bool { return page.Workspaces[i].ID < page.Workspaces[j].ID })
	}

	page.Email = extractOpenCodeGoEmail(tokens)
	page.ReferralCode, _ = findOpenCodeGoSSRStringProperty(tokens, "referralCode")
	page.AvailableReferralRewards, page.UsedReferralRewards = countOpenCodeGoReferralRewards(tokens)
	page.MembershipStatus, page.SubscriptionReference = parseOpenCodeGoMembership(tokens, visibleText)

	if page.MembershipStatus != model.OpenCodeGoMembershipActive {
		if page.MembershipStatus == model.OpenCodeGoMembershipUnknown {
			page.QuotaParseError = "membership status is missing from the console snapshot"
		}
		return page, nil
	}

	quota, quotaErr := parseOpenCodeGoAuthoritativeQuota(tokens, fetchedAt)
	if quotaErr != nil {
		page.QuotaParseError = quotaErr.Error()
		return page, nil
	}
	page.Quota = quota
	return page, nil
}

func ParseOpenCodeGoAPIKeyPage(document string) (string, error) {
	if len(document) == 0 {
		return "", errors.New("OpenCode Go API-key page is empty")
	}
	if len(document) > openCodeGoMaxSSRDocumentSize {
		return "", errors.New("OpenCode Go API-key page is too large")
	}
	doc, err := html.Parse(strings.NewReader(document))
	if err != nil {
		return "", errors.New("OpenCode Go API-key page could not be parsed")
	}
	scriptSource, _, _ := extractOpenCodeGoConsoleHTML(doc)
	tokens := lexOpenCodeGoSSR(scriptSource)
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].kind != openCodeGoSSRTokenIdentifier || tokens[index].value != "key" || tokens[index+1].value != ":" || tokens[index+2].kind != openCodeGoSSRTokenString {
			continue
		}
		candidate := tokens[index+2].value
		if strings.HasPrefix(candidate, "sk-") && len(candidate) <= 4096 && !strings.ContainsAny(candidate, "\r\n\t ") {
			return candidate, nil
		}
	}
	return "", nil
}

func extractOpenCodeGoConsoleHTML(doc *html.Node) (string, string, *bool) {
	var scripts strings.Builder
	var visible strings.Builder
	var chinaModelsEnabled *bool

	var walk func(*html.Node, bool)
	walk = func(node *html.Node, inScript bool) {
		nextInScript := inScript || (node.Type == html.ElementNode && strings.EqualFold(node.Data, "script"))
		if node.Type == html.TextNode {
			if nextInScript {
				scripts.WriteString(node.Data)
				scripts.WriteByte('\n')
			} else {
				visible.WriteString(node.Data)
				visible.WriteByte(' ')
			}
		}
		if chinaModelsEnabled == nil && node.Type == html.ElementNode && strings.EqualFold(node.Data, "form") {
			chinaModelsEnabled = extractOpenCodeGoChinaModelForm(node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, nextInScript)
		}
	}
	walk(doc, false)
	return scripts.String(), visible.String(), chinaModelsEnabled
}

func extractOpenCodeGoChinaModelForm(form *html.Node) *bool {
	var result *bool
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if result != nil {
			return
		}
		if node.Type == html.ElementNode && strings.EqualFold(node.Data, "input") {
			name := openCodeGoHTMLAttribute(node, "name")
			if name == "useChinaProviders" {
				value := openCodeGoHTMLAttribute(node, "value")
				if parsed, err := strconv.ParseBool(value); err == nil {
					result = &parsed
				}
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(form)
	return result
}

func openCodeGoHTMLAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val
		}
	}
	return ""
}

func lexOpenCodeGoSSR(source string) []openCodeGoSSRToken {
	tokens := make([]openCodeGoSSRToken, 0, len(source)/8)
	for position := 0; position < len(source) && len(tokens) <= openCodeGoMaxSSRTokens; {
		char := source[position]
		if isOpenCodeGoSSRSpace(char) {
			position++
			continue
		}
		if char == '/' && position+1 < len(source) {
			switch source[position+1] {
			case '/':
				position += 2
				for position < len(source) && source[position] != '\n' {
					position++
				}
				continue
			case '*':
				position += 2
				for position+1 < len(source) && !(source[position] == '*' && source[position+1] == '/') {
					position++
				}
				if position+1 < len(source) {
					position += 2
				}
				continue
			}
		}
		if char == '"' || char == '\'' {
			value, next := scanOpenCodeGoSSRString(source, position)
			tokens = append(tokens, openCodeGoSSRToken{kind: openCodeGoSSRTokenString, value: value})
			position = next
			continue
		}
		if isOpenCodeGoSSRIdentifierStart(char) {
			start := position
			position++
			for position < len(source) && isOpenCodeGoSSRIdentifierPart(source[position]) {
				position++
			}
			tokens = append(tokens, openCodeGoSSRToken{kind: openCodeGoSSRTokenIdentifier, value: source[start:position]})
			continue
		}
		if isOpenCodeGoSSRNumberStart(source, position) {
			start := position
			position = scanOpenCodeGoSSRNumber(source, position)
			tokens = append(tokens, openCodeGoSSRToken{kind: openCodeGoSSRTokenNumber, value: source[start:position]})
			continue
		}
		tokens = append(tokens, openCodeGoSSRToken{kind: openCodeGoSSRTokenPunctuation, value: string(char)})
		position++
	}
	return tokens
}

func scanOpenCodeGoSSRString(source string, start int) (string, int) {
	quote := source[start]
	var value strings.Builder
	for position := start + 1; position < len(source); position++ {
		char := source[position]
		if char == quote {
			return value.String(), position + 1
		}
		if char != '\\' || position+1 >= len(source) {
			value.WriteByte(char)
			continue
		}
		position++
		escaped := source[position]
		switch escaped {
		case '\\', '/', '"', '\'':
			value.WriteByte(escaped)
		case 'n':
			value.WriteByte('\n')
		case 'r':
			value.WriteByte('\r')
		case 't':
			value.WriteByte('\t')
		case 'b':
			value.WriteByte('\b')
		case 'f':
			value.WriteByte('\f')
		case 'u':
			if position+4 < len(source) {
				if code, err := strconv.ParseUint(source[position+1:position+5], 16, 16); err == nil {
					value.WriteRune(rune(code))
					position += 4
					continue
				}
			}
			value.WriteByte(escaped)
		default:
			value.WriteByte(escaped)
		}
	}
	return value.String(), len(source)
}

func scanOpenCodeGoSSRNumber(source string, position int) int {
	if source[position] == '+' || source[position] == '-' {
		position++
	}
	for position < len(source) && source[position] >= '0' && source[position] <= '9' {
		position++
	}
	if position < len(source) && source[position] == '.' {
		position++
		for position < len(source) && source[position] >= '0' && source[position] <= '9' {
			position++
		}
	}
	if position < len(source) && (source[position] == 'e' || source[position] == 'E') {
		position++
		if position < len(source) && (source[position] == '+' || source[position] == '-') {
			position++
		}
		for position < len(source) && source[position] >= '0' && source[position] <= '9' {
			position++
		}
	}
	return position
}

func isOpenCodeGoSSRSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

func isOpenCodeGoSSRIdentifierStart(char byte) bool {
	return char == '_' || char == '$' || (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

func isOpenCodeGoSSRIdentifierPart(char byte) bool {
	return isOpenCodeGoSSRIdentifierStart(char) || (char >= '0' && char <= '9')
}

func isOpenCodeGoSSRNumberStart(source string, position int) bool {
	char := source[position]
	if char >= '0' && char <= '9' {
		return true
	}
	return (char == '+' || char == '-') && position+1 < len(source) && source[position+1] >= '0' && source[position+1] <= '9'
}

func extractOpenCodeGoWorkspaces(tokens []openCodeGoSSRToken) []OpenCodeGoDiscoveredWorkspace {
	objects := collectOpenCodeGoSSRObjects(tokens)
	workspaces := make(map[string]OpenCodeGoDiscoveredWorkspace)
	for _, object := range objects {
		id, ok := object["id"]
		if !ok || id.kind != openCodeGoSSRTokenString || !openCodeGoWorkspaceIDPattern.MatchString(id.value) {
			continue
		}
		workspace := OpenCodeGoDiscoveredWorkspace{ID: id.value}
		if name, exists := object["name"]; exists && name.kind == openCodeGoSSRTokenString {
			workspace.Name = name.value
		}
		if _, exists := workspaces[workspace.ID]; !exists {
			workspaces[workspace.ID] = workspace
		}
	}
	result := make([]OpenCodeGoDiscoveredWorkspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		result = append(result, workspace)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func containsOpenCodeGoWorkspace(workspaces []OpenCodeGoDiscoveredWorkspace, workspaceID string) bool {
	for _, workspace := range workspaces {
		if strings.EqualFold(workspace.ID, workspaceID) {
			return true
		}
	}
	return false
}

func collectOpenCodeGoSSRObjects(tokens []openCodeGoSSRToken) []map[string]openCodeGoSSRToken {
	type frame struct {
		fields map[string]openCodeGoSSRToken
	}
	stack := make([]frame, 0)
	objects := make([]map[string]openCodeGoSSRToken, 0)
	for index, token := range tokens {
		if token.kind == openCodeGoSSRTokenPunctuation {
			switch token.value {
			case "{":
				stack = append(stack, frame{fields: make(map[string]openCodeGoSSRToken)})
				continue
			case "}":
				if len(stack) > 0 {
					objects = append(objects, stack[len(stack)-1].fields)
					stack = stack[:len(stack)-1]
				}
				continue
			}
		}
		if len(stack) == 0 || (token.kind != openCodeGoSSRTokenIdentifier && token.kind != openCodeGoSSRTokenString) {
			continue
		}
		if index+2 >= len(tokens) || tokens[index+1].value != ":" || !isOpenCodeGoSSRScalar(tokens[index+2]) {
			continue
		}
		stack[len(stack)-1].fields[token.value] = tokens[index+2]
	}
	return objects
}

func findOpenCodeGoSSRStringProperty(tokens []openCodeGoSSRToken, property string) (string, bool) {
	token, found := findOpenCodeGoSSRScalarProperty(tokens, property)
	if !found || token.kind != openCodeGoSSRTokenString {
		return "", false
	}
	return token.value, true
}

func findOpenCodeGoSSRScalarProperty(tokens []openCodeGoSSRToken, property string) (openCodeGoSSRToken, bool) {
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].kind != openCodeGoSSRTokenIdentifier || tokens[index].value != property || tokens[index+1].value != ":" {
			continue
		}
		if isOpenCodeGoSSRScalar(tokens[index+2]) {
			return tokens[index+2], true
		}
	}
	return openCodeGoSSRToken{}, false
}

func isOpenCodeGoSSRScalar(token openCodeGoSSRToken) bool {
	return token.kind == openCodeGoSSRTokenString || token.kind == openCodeGoSSRTokenNumber || token.kind == openCodeGoSSRTokenIdentifier
}

func extractOpenCodeGoEmail(tokens []openCodeGoSSRToken) string {
	if value, found := findOpenCodeGoSSRStringProperty(tokens, "email"); found && isOpenCodeGoEmail(value) {
		return value
	}
	for _, token := range tokens {
		if token.kind == openCodeGoSSRTokenString && isOpenCodeGoEmail(token.value) {
			return token.value
		}
	}
	return ""
}

func isOpenCodeGoEmail(value string) bool {
	if len(value) == 0 || len(value) > 254 || !strings.Contains(value, "@") {
		return false
	}
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value
}

func parseOpenCodeGoMembership(tokens []openCodeGoSSRToken, visibleText string) (string, string) {
	if status, found := findOpenCodeGoSSRStringProperty(tokens, "subscriptionStatus"); found {
		switch strings.ToLower(status) {
		case "active", "trialing":
			return model.OpenCodeGoMembershipActive, ""
		case "inactive", "expired", "canceled", "cancelled":
			return model.OpenCodeGoMembershipInactive, ""
		}
	}
	if reference, found := findOpenCodeGoSSRScalarProperty(tokens, "liteSubscriptionID"); found {
		if reference.kind == openCodeGoSSRTokenString && reference.value != "" {
			return model.OpenCodeGoMembershipActive, reference.value
		}
		if reference.kind == openCodeGoSSRTokenIdentifier && reference.value == "null" {
			return model.OpenCodeGoMembershipInactive, ""
		}
	}
	if strings.Contains(visibleText, "您已订阅 OpenCode Go") || strings.Contains(strings.ToLower(visibleText), "subscribed to opencode go") {
		return model.OpenCodeGoMembershipActive, ""
	}
	return model.OpenCodeGoMembershipUnknown, ""
}

func countOpenCodeGoReferralRewards(tokens []openCodeGoSSRToken) (int, int) {
	available := make(map[string]struct{})
	used := make(map[string]struct{})
	for _, object := range collectOpenCodeGoSSRObjects(tokens) {
		id, hasID := object["id"]
		source, hasSource := object["source"]
		status, hasStatus := object["status"]
		if !hasID || !hasSource || !hasStatus || id.kind != openCodeGoSSRTokenString || source.kind != openCodeGoSSRTokenString || status.kind != openCodeGoSSRTokenString {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(id.value), "ref_") || (source.value != "inviter" && source.value != "invitee") {
			continue
		}
		switch status.value {
		case "available":
			available[id.value] = struct{}{}
		case "applied", "used":
			used[id.value] = struct{}{}
		}
	}
	return len(available), len(used)
}

func parseOpenCodeGoAuthoritativeQuota(tokens []openCodeGoSSRToken, fetchedAt time.Time) (*OpenCodeGoAuthoritativeQuotaSnapshot, error) {
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}
	windows := make([]OpenCodeGoAuthoritativeQuotaWindow, 0, len(model.OpenCodeGoQuotaKinds))
	nextRefreshAt := int64(0)
	for _, kind := range model.OpenCodeGoQuotaKinds {
		object, found := findOpenCodeGoSSRObjectProperty(tokens, kind+"Usage")
		if !found {
			return nil, fmt.Errorf("%s quota window is missing", kind)
		}
		usedPercent, err := parseOpenCodeGoSSRFiniteFloat(object["usagePercent"])
		if err != nil || usedPercent < 0 {
			return nil, fmt.Errorf("%s quota usagePercent is invalid", kind)
		}
		resetSeconds, err := parseOpenCodeGoSSRResetSeconds(object["resetInSec"])
		if err != nil {
			return nil, fmt.Errorf("%s quota resetInSec is invalid", kind)
		}
		resetAt := fetchedAt.Unix() + resetSeconds
		if resetAt < fetchedAt.Unix() {
			return nil, fmt.Errorf("%s quota resetAt is invalid", kind)
		}
		windows = append(windows, OpenCodeGoAuthoritativeQuotaWindow{
			Kind:         kind,
			UsedPercent:  usedPercent,
			ResetSeconds: resetSeconds,
			ResetAt:      resetAt,
			FetchedAt:    fetchedAt.Unix(),
		})
		if nextRefreshAt == 0 || resetAt < nextRefreshAt {
			nextRefreshAt = resetAt
		}
	}
	return &OpenCodeGoAuthoritativeQuotaSnapshot{
		Windows:       windows,
		FetchedAt:     fetchedAt.Unix(),
		NextRefreshAt: nextRefreshAt,
	}, nil
}

func findOpenCodeGoSSRObjectProperty(tokens []openCodeGoSSRToken, property string) (map[string]openCodeGoSSRToken, bool) {
	for index := 0; index+2 < len(tokens); index++ {
		if tokens[index].kind != openCodeGoSSRTokenIdentifier || tokens[index].value != property || tokens[index+1].value != ":" {
			continue
		}
		limit := index + 14
		if limit > len(tokens) {
			limit = len(tokens)
		}
		for objectIndex := index + 2; objectIndex < limit; objectIndex++ {
			if tokens[objectIndex].value == "{" {
				object, _, ok := parseOpenCodeGoSSRObjectAt(tokens, objectIndex)
				return object, ok
			}
			if tokens[objectIndex].value == "," || tokens[objectIndex].value == ";" {
				break
			}
		}
	}
	return nil, false
}

func parseOpenCodeGoSSRObjectAt(tokens []openCodeGoSSRToken, start int) (map[string]openCodeGoSSRToken, int, bool) {
	if start >= len(tokens) || tokens[start].value != "{" {
		return nil, start, false
	}
	object := make(map[string]openCodeGoSSRToken)
	depth := 0
	for index := start; index < len(tokens); index++ {
		switch tokens[index].value {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return object, index, true
			}
		}
		if depth != 1 || index+2 >= len(tokens) || tokens[index+1].value != ":" || !isOpenCodeGoSSRScalar(tokens[index+2]) {
			continue
		}
		if tokens[index].kind == openCodeGoSSRTokenIdentifier || tokens[index].kind == openCodeGoSSRTokenString {
			object[tokens[index].value] = tokens[index+2]
		}
	}
	return nil, len(tokens), false
}

func parseOpenCodeGoSSRFiniteFloat(token openCodeGoSSRToken) (float64, error) {
	if token.kind != openCodeGoSSRTokenNumber {
		return 0, errors.New("not a number")
	}
	value, err := strconv.ParseFloat(token.value, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, errors.New("not a finite number")
	}
	return value, nil
}

func parseOpenCodeGoSSRResetSeconds(token openCodeGoSSRToken) (int64, error) {
	value, err := parseOpenCodeGoSSRFiniteFloat(token)
	if err != nil || value < 0 || value > float64(openCodeGoMaxResetSeconds) || math.Trunc(value) != value {
		return 0, errors.New("not a bounded non-negative integer")
	}
	return int64(value), nil
}
