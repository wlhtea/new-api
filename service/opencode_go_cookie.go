package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	openCodeGoAuthCookieName = "auth"
	openCodeGoLocaleCookie   = "oc_locale=zh"
	openCodeGoMaxCookieSize  = 16 * 1024
)

func NormalizeOpenCodeGoAuthCookie(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", errors.New("OpenCode Go auth Cookie is required")
	}
	if len(value) > openCodeGoMaxCookieSize {
		return "", errors.New("OpenCode Go auth Cookie is too large")
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", errors.New("OpenCode Go auth Cookie contains a line break")
	}
	if len(value) >= len("cookie:") && strings.EqualFold(value[:len("cookie:")], "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}

	if strings.Contains(value, ";") || hasOpenCodeGoAuthCookiePrefix(value) {
		request := &http.Request{Header: http.Header{"Cookie": []string{value}}}
		var authValues []string
		for _, cookie := range request.Cookies() {
			if cookie.Name == openCodeGoAuthCookieName {
				authValues = append(authValues, cookie.Value)
			}
		}
		if len(authValues) != 1 || authValues[0] == "" {
			return "", errors.New("OpenCode Go Cookie header must contain exactly one non-empty auth value")
		}
		value = authValues[0]
	}

	if err := validateOpenCodeGoCookieOctets(value); err != nil {
		return "", err
	}
	return value, nil
}

func ParseOpenCodeGoAuthCookieLines(input string) ([]string, error) {
	values := make([]string, 0)
	seen := make(map[string]struct{})
	for index, line := range strings.Split(strings.ReplaceAll(input, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		value, err := NormalizeOpenCodeGoAuthCookie(line)
		if err != nil {
			return nil, fmt.Errorf("OpenCode Go auth Cookie line %d: %w", index+1, err)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, errors.New("at least one OpenCode Go auth Cookie is required")
	}
	return values, nil
}

func BuildOpenCodeGoCookieHeader(authCookie string) (string, error) {
	value, err := NormalizeOpenCodeGoAuthCookie(authCookie)
	if err != nil {
		return "", err
	}
	return openCodeGoAuthCookieName + "=" + value + "; " + openCodeGoLocaleCookie, nil
}

func hasOpenCodeGoAuthCookiePrefix(value string) bool {
	if len(value) < len(openCodeGoAuthCookieName)+1 {
		return false
	}
	return strings.EqualFold(value[:len(openCodeGoAuthCookieName)+1], openCodeGoAuthCookieName+"=")
}

func validateOpenCodeGoCookieOctets(value string) error {
	if value == "" {
		return errors.New("OpenCode Go auth Cookie is empty")
	}
	for _, char := range []byte(value) {
		valid := char == 0x21 ||
			(char >= 0x23 && char <= 0x2b) ||
			(char >= 0x2d && char <= 0x3a) ||
			(char >= 0x3c && char <= 0x5b) ||
			(char >= 0x5d && char <= 0x7e)
		if !valid {
			return errors.New("OpenCode Go auth Cookie contains an invalid character")
		}
	}
	return nil
}
