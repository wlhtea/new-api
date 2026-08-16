package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/QuantumNous/new-api/common"
)

type jsonStringContainer struct {
	object       bool
	expectingKey bool
}

// VisitJSONStringValues visits decoded JSON string values while excluding
// object member names. It uses an iterative token stream so candidate security
// scans do not recurse with attacker-controlled nesting.
func VisitJSONStringValues(ctx context.Context, document []byte, visit func(string) error) error {
	if visit == nil {
		return errors.New("JSON string visitor is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	decoder := common.NewJsonDecoderUseNumber(bytes.NewReader(document))
	containers := make([]jsonStringContainer, 0, 8)
	rootSeen := false
	rootComplete := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode finalized JSON token: %w", err)
		}
		if rootComplete && len(containers) == 0 {
			return errors.New("finalized JSON contains a trailing value")
		}

		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				if !rootSeen {
					rootSeen = true
				} else {
					markJSONStringValueConsumed(containers)
				}
				containers = append(containers, jsonStringContainer{
					object:       value == '{',
					expectingKey: value == '{',
				})
			case '}', ']':
				if len(containers) == 0 || containers[len(containers)-1].object != (value == '}') {
					return errors.New("finalized JSON container state is invalid")
				}
				containers = containers[:len(containers)-1]
				if len(containers) == 0 {
					rootComplete = true
				}
			default:
				return errors.New("finalized JSON delimiter is invalid")
			}
		case string:
			if len(containers) > 0 && containers[len(containers)-1].object && containers[len(containers)-1].expectingKey {
				containers[len(containers)-1].expectingKey = false
				continue
			}
			if err := visit(value); err != nil {
				return err
			}
			if !rootSeen {
				rootSeen = true
				rootComplete = true
			} else {
				markJSONStringValueConsumed(containers)
			}
		default:
			if !rootSeen {
				rootSeen = true
				rootComplete = true
			} else {
				markJSONStringValueConsumed(containers)
			}
		}
	}

	if !rootSeen || !rootComplete || len(containers) != 0 {
		return errors.New("finalized JSON document is incomplete")
	}
	return nil
}

func markJSONStringValueConsumed(containers []jsonStringContainer) {
	if len(containers) == 0 {
		return
	}
	index := len(containers) - 1
	if containers[index].object {
		containers[index].expectingKey = true
	}
}
