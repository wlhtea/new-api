package dto

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// OpenCodeGoIdentityProxyTemplate is a validated IPWO-style proxy username
// template. Its fields stay private so callers cannot accidentally serialize
// credentials or a derived session ID.
type OpenCodeGoIdentityProxyTemplate struct {
	proxyURL  *url.URL
	username  []string
	password  string
	zoneIndex int
	sidIndex  int
	timeIndex int
	country   string
	minutes   int
}

// ParseOpenCodeGoIdentityProxyTemplate validates a credentialed HTTP(S) proxy
// and locates the underscore-delimited zone, sid, and time components.
func ParseOpenCodeGoIdentityProxyTemplate(rawProxyURL string) (*OpenCodeGoIdentityProxyTemplate, error) {
	trimmedProxyURL := strings.TrimSpace(rawProxyURL)
	if strings.Contains(trimmedProxyURL, "#") {
		return nil, errors.New("identity proxy URL must not include a fragment")
	}
	parsedURL, err := url.Parse(trimmedProxyURL)
	if err != nil {
		return nil, errors.New("identity proxy URL is invalid")
	}
	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, errors.New("identity proxy URL must use http or https")
	}
	if parsedURL.Hostname() == "" {
		return nil, errors.New("identity proxy URL must include a host")
	}
	if parsedURL.Path != "" && parsedURL.Path != "/" {
		return nil, errors.New("identity proxy URL must not include a path")
	}
	if parsedURL.RawQuery != "" || parsedURL.ForceQuery {
		return nil, errors.New("identity proxy URL must not include a query")
	}
	if parsedURL.Fragment != "" {
		return nil, errors.New("identity proxy URL must not include a fragment")
	}
	if parsedURL.User == nil || strings.TrimSpace(parsedURL.User.Username()) == "" {
		return nil, errors.New("identity proxy URL must include a username")
	}
	password, hasPassword := parsedURL.User.Password()
	if !hasPassword || password == "" {
		return nil, errors.New("identity proxy URL must include a password")
	}

	template := &OpenCodeGoIdentityProxyTemplate{
		proxyURL:  parsedURL,
		username:  strings.Split(parsedURL.User.Username(), "_"),
		password:  password,
		zoneIndex: -1,
		sidIndex:  -1,
		timeIndex: -1,
	}
	if err := template.parseComponents(); err != nil {
		return nil, err
	}
	if template.sidIndex < 0 {
		return nil, errors.New("identity proxy username must contain exactly one sid component")
	}
	return template, nil
}

func (template *OpenCodeGoIdentityProxyTemplate) parseComponents() error {
	parts := template.username
	for index := 0; index < len(parts); index++ {
		switch strings.ToLower(parts[index]) {
		case "custom":
			if index+1 < len(parts) && strings.EqualFold(parts[index+1], "zone") {
				if index+2 >= len(parts) || !isASCIIAlphaCountry(normalizeIdentityProxyCountry(parts[index+2])) {
					return errors.New("identity proxy username contains a malformed zone component")
				}
				if template.zoneIndex >= 0 {
					return errors.New("identity proxy username contains multiple zone components")
				}
				template.zoneIndex = index + 2
				template.country = parts[index+2]
				index += 2
			}
		case "zone":
			if index+1 >= len(parts) || !isASCIIAlphaCountry(normalizeIdentityProxyCountry(parts[index+1])) {
				return errors.New("identity proxy username contains a malformed zone component")
			}
			if template.zoneIndex >= 0 {
				return errors.New("identity proxy username contains multiple zone components")
			}
			template.zoneIndex = index + 1
			template.country = parts[index+1]
			index++
		case "sid":
			if index+1 >= len(parts) || !validOpenCodeGoIdentityProxyValue(parts[index+1]) {
				return errors.New("identity proxy username contains a malformed sid component")
			}
			if template.sidIndex >= 0 {
				return errors.New("identity proxy username contains multiple sid components")
			}
			template.sidIndex = index + 1
			index++
		case "time":
			if index+1 >= len(parts) || !validOpenCodeGoIdentityProxyValue(parts[index+1]) {
				return errors.New("identity proxy username contains a malformed time component")
			}
			if template.timeIndex >= 0 {
				return errors.New("identity proxy username contains multiple time components")
			}
			minutes, err := strconv.Atoi(parts[index+1])
			if err != nil || minutes < OpenCodeGoIdentityProxyMinRotateMinutes || minutes > OpenCodeGoIdentityProxyMaxRotateMinutes {
				return fmt.Errorf(
					"identity proxy time must be between %d and %d minutes",
					OpenCodeGoIdentityProxyMinRotateMinutes,
					OpenCodeGoIdentityProxyMaxRotateMinutes,
				)
			}
			template.timeIndex = index + 1
			template.minutes = minutes
			index++
		}
	}
	return nil
}

func validOpenCodeGoIdentityProxyValue(value string) bool {
	if value == "" {
		return false
	}
	switch strings.ToLower(value) {
	case "custom", "zone", "sid", "time":
		return false
	default:
		return true
	}
}

// InferredPolicy returns provider values present in the template. Missing
// components are returned as zero values so the caller can apply defaults.
func (template *OpenCodeGoIdentityProxyTemplate) InferredPolicy() (string, int) {
	if template == nil {
		return "", 0
	}
	return normalizeIdentityProxyCountry(template.country), template.minutes
}

// Rewrite returns a proxy URL whose template SID, country, and lifetime are
// replaced for one identity binding. Endpoint and password remain unchanged.
func (template *OpenCodeGoIdentityProxyTemplate) Rewrite(country string, sid string, minutes int) (string, error) {
	if template == nil || template.proxyURL == nil {
		return "", errors.New("identity proxy template is unavailable")
	}
	country = normalizeIdentityProxyCountry(country)
	if !isASCIIAlphaCountry(country) {
		return "", errors.New("identity proxy country must contain exactly two ASCII letters")
	}
	if sid == "" {
		return "", errors.New("identity proxy sid is empty")
	}
	for _, char := range sid {
		if char < '0' || char > '9' {
			return "", errors.New("identity proxy sid must be numeric")
		}
	}
	if minutes < OpenCodeGoIdentityProxyMinRotateMinutes || minutes > OpenCodeGoIdentityProxyMaxRotateMinutes {
		return "", fmt.Errorf(
			"identity proxy rotation must be between %d and %d minutes",
			OpenCodeGoIdentityProxyMinRotateMinutes,
			OpenCodeGoIdentityProxyMaxRotateMinutes,
		)
	}

	parts := append([]string(nil), template.username...)
	parts[template.sidIndex] = sid
	if template.zoneIndex >= 0 {
		parts[template.zoneIndex] = country
	} else {
		parts = append(parts, "zone", country)
	}
	if template.timeIndex >= 0 {
		parts[template.timeIndex] = strconv.Itoa(minutes)
	} else {
		parts = append(parts, "time", strconv.Itoa(minutes))
	}

	derivedURL := *template.proxyURL
	derivedURL.User = url.UserPassword(strings.Join(parts, "_"), template.password)
	derivedURL.Path = ""
	derivedURL.RawPath = ""
	return derivedURL.String(), nil
}
