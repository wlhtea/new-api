package helper

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisitJSONStringValuesExcludesKeysAndPreservesDocumentOrder(t *testing.T) {
	var values []string
	err := VisitJSONStringValues(
		context.Background(),
		[]byte(`{"secret-key":"first","nested":{"other-key":["second",3,null,true]},"last":"third"}`),
		func(value string) error {
			values = append(values, value)
			return nil
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second", "third"}, values)
}

func TestVisitJSONStringValuesRejectsTrailingDocument(t *testing.T) {
	err := VisitJSONStringValues(context.Background(), []byte(`{"ok":"value"} {}`), func(string) error { return nil })
	require.Error(t, err)
}

func TestVisitJSONStringValuesHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := VisitJSONStringValues(ctx, []byte(`{"ok":"value"}`), func(string) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}
