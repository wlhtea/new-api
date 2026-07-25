package seedance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
)

type imageCandidate struct {
	source string
	bytes  []byte
	mime   string
}

type remoteImageLoader func(string) (mimeType string, data string, err error)

func validateImageCandidate(candidate imageCandidate) (imageCandidate, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(candidate.bytes))
	if err != nil {
		return imageCandidate{}, fmt.Errorf("decode image config: %w", err)
	}
	detected := map[string]string{
		"jpeg": "image/jpeg",
		"png":  "image/png",
	}[format]
	if detected == "" || (candidate.mime != "" && candidate.mime != detected) {
		return imageCandidate{}, errors.New("image must be JPEG or PNG")
	}
	if config.Width < 240 || config.Height < 240 ||
		config.Width > 8000 || config.Height > 8000 {
		return imageCandidate{}, errors.New("image dimensions must be 240–8000")
	}
	ratio := float64(config.Width) / float64(config.Height)
	if ratio < 1.0/8.0 || ratio > 8.0 {
		return imageCandidate{}, errors.New("image ratio must be between 1:8 and 8:1")
	}
	candidate.mime = detected
	return candidate, nil
}

func loadImageCandidate(source string, loadRemote remoteImageLoader) (imageCandidate, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return imageCandidate{}, nil
	}

	mimeType := ""
	encoded := source
	switch {
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		var err error
		mimeType, encoded, err = loadRemote(source)
		if err != nil {
			return imageCandidate{}, err
		}
	case strings.HasPrefix(source, "data:image/jpeg;base64,"):
		mimeType = "image/jpeg"
		encoded = strings.TrimPrefix(source, "data:image/jpeg;base64,")
	case strings.HasPrefix(source, "data:image/png;base64,"):
		mimeType = "image/png"
		encoded = strings.TrimPrefix(source, "data:image/png;base64,")
	case strings.HasPrefix(source, "data:"):
		return imageCandidate{}, errors.New("unsupported image data URI")
	}

	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return imageCandidate{}, fmt.Errorf("decode image: %w", err)
	}
	return validateImageCandidate(imageCandidate{
		source: source,
		bytes:  decoded,
		mime:   mimeType,
	})
}

func imageStrings(input *requestInput) ([]string, error) {
	sources := make([]string, 0, 4)
	for _, raw := range []json.RawMessage{
		input.Raw.Image,
		input.Raw.InputReference,
		input.Metadata.ImageBase64,
	} {
		value, present, err := parseStringValue(raw)
		if err != nil {
			return nil, err
		}
		if present && strings.TrimSpace(value) != "" {
			sources = append(sources, value)
		}
	}

	imagesRaw := bytes.TrimSpace(input.Raw.Images)
	if len(imagesRaw) == 0 || bytes.Equal(imagesRaw, []byte("null")) {
		return sources, nil
	}
	var images []json.RawMessage
	if err := json.Unmarshal(imagesRaw, &images); err != nil {
		return nil, err
	}
	if len(images) > 1 {
		return nil, errors.New("images must contain at most one item")
	}
	if len(images) == 1 {
		value, present, err := parseStringValue(images[0])
		if err != nil {
			return nil, err
		}
		if present && strings.TrimSpace(value) != "" {
			sources = append(sources, value)
		}
	}
	return sources, nil
}

func normalizeImages(
	input *requestInput,
	uploaded *imageCandidate,
	loadRemote remoteImageLoader,
) (string, *dto.TaskError) {
	sources, err := imageStrings(input)
	if err != nil {
		return "", service.TaskErrorWrapperLocal(err, "invalid_image", http.StatusBadRequest)
	}

	candidates := make([]imageCandidate, 0, len(sources)+1)
	for _, source := range sources {
		candidate, err := loadImageCandidate(source, loadRemote)
		if err != nil {
			return "", service.TaskErrorWrapperLocal(err, "invalid_image", http.StatusBadRequest)
		}
		if len(candidate.bytes) != 0 {
			candidates = append(candidates, candidate)
		}
	}
	if uploaded != nil {
		candidate, err := validateImageCandidate(*uploaded)
		if err != nil {
			return "", service.TaskErrorWrapperLocal(err, "invalid_image", http.StatusBadRequest)
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return "", nil
	}

	canonical := candidates[0].bytes
	canonicalHash := sha256.Sum256(canonical)
	for _, candidate := range candidates[1:] {
		if sha256.Sum256(candidate.bytes) != canonicalHash {
			return "", service.TaskErrorWrapperLocal(
				errors.New("multiple different images are not supported"),
				"invalid_image",
				http.StatusBadRequest,
			)
		}
	}
	return base64.StdEncoding.EncodeToString(canonical), nil
}
