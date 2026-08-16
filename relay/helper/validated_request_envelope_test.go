package helper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatedRequestEnvelopeRejectsRecursiveDecodedDuplicateKeys(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top level exact duplicate",
			body: `{"model":"test-model","model":"other","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "top level escaped alias",
			body: `{"model":"test-model","\u006dodel":"other","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "nested escaped alias",
			body: `{"model":"test-model","messages":[{"role":"user","content":"hi","metadata":{"name":1,"n\u0061me":2}}]}`,
		},
		{
			name: "array nested duplicate",
			body: `{"model":"test-model","messages":[{"role":"user","content":[{"type":"text","text":"hi","text":"bye"}]}]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, "/v1/messages", []byte(test.body))

			_, err := GetAndValidateRequest(c, types.RelayFormatClaude)

			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			assert.Equal(t, "json.duplicate_key", validationErr.RuleID)
			assert.NotContains(t, validationErr.Error(), "metadata")
			assert.NotContains(t, validationErr.Error(), "name")
		})
	}
}

func TestValidatedRequestEnvelopeRejectsUnpairedSurrogatesAndAcceptsPairs(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "high surrogate value", body: `{"model":"test-model","messages":[{"role":"user","content":"\uD800"}]}`},
		{name: "low surrogate value", body: `{"model":"test-model","messages":[{"role":"user","content":"\uDC00"}]}`},
		{name: "invalid surrogate pair value", body: `{"model":"test-model","messages":[{"role":"user","content":"\uD800\u0041"}]}`},
		{name: "high surrogate key", body: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"\uD800":1}`},
		{name: "low surrogate key", body: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"\uDC00":1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newRelayValidationContext(t, "/v1/messages", []byte(test.body))
			_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			assert.Equal(t, "json.unpaired_surrogate", validationErr.RuleID)
		})
	}

	valid := newRelayValidationContext(t, "/v1/messages", []byte(
		`{"model":"test-model","messages":[{"role":"user","content":"\uD83D\uDE00"}]}`,
	))
	_, err := GetAndValidateRequest(valid, types.RelayFormatClaude)
	require.NoError(t, err)
}

func TestValidatedRequestEnvelopePreservesSpansPresenceAndDecodedPaths(t *testing.T) {
	body := []byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"hi"}],
		"stream":false,
		"zero":0,
		"big":900719925474099312345678901234567890,
		"explicit_false":false,
		"empty_string":"",
		"empty_object":{},
		"empty_array":[],
		"explicit_null":null,
		"a.b":1,
		"a/b":2,
		"a~b":3,
		"~1/slash":4,
		"nested":{"a/b":{"~key":[{"":0}]}}
	}`)
	c := newRelayValidationContext(t, "/v1/messages", body)

	_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, envelope)

	present, stream, validBoolean := envelope.Stream()
	assert.True(t, present)
	assert.False(t, stream)
	assert.True(t, validBoolean)
	assert.Equal(t, int64(len(body)), envelope.DecodedBytes())
	assert.NotEmpty(t, envelope.BodyFingerprint())
	assert.NotEmpty(t, envelope.InventoryFingerprint())
	assert.NotEmpty(t, envelope.ContractFingerprint())
	rawBig, exists, err := envelope.RawTopLevelField("big")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Equal(t, "900719925474099312345678901234567890", string(rawBig))
	for name, want := range map[string]string{
		"zero":           "0",
		"explicit_false": "false",
		"empty_string":   `""`,
		"empty_object":   "{}",
		"empty_array":    "[]",
		"explicit_null":  "null",
	} {
		raw, ok, readErr := envelope.RawTopLevelField(name)
		require.NoError(t, readErr)
		require.True(t, ok)
		assert.Equal(t, want, string(raw))
	}

	pointers := make(map[string]JSONValueKind)
	for _, entry := range envelope.Inventory() {
		pointers[entry.Pointer] = entry.Kind
	}
	assert.Equal(t, JSONValueNumber, pointers["/a.b"])
	assert.Equal(t, JSONValueNumber, pointers["/a~1b"])
	assert.Equal(t, JSONValueNumber, pointers["/a~0b"])
	assert.Equal(t, JSONValueNumber, pointers["/~01~1slash"])
	assert.Equal(t, JSONValueNumber, pointers["/nested/a~1b/~0key/0/"])
	assert.Equal(t, JSONValueObject, pointers[""])

	var bigEntry JSONInventoryEntry
	for _, entry := range envelope.Inventory() {
		if entry.Pointer == "/big" {
			bigEntry = entry
			break
		}
	}
	require.Greater(t, bigEntry.End, bigEntry.Start)
	require.LessOrEqual(t, bigEntry.End, int64(len(body)))
	assert.Equal(t, string(rawBig), string(body[bigEntry.Start:bigEntry.End]))

	absentContext := newRelayValidationContext(t, "/v1/messages", []byte(
		`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
	))
	_, err = GetAndValidateRequest(absentContext, types.RelayFormatClaude)
	require.NoError(t, err)
	absentEnvelope, found, err := GetValidatedRequestEnvelope(absentContext, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)
	present, _, _ = absentEnvelope.Stream()
	assert.False(t, present)
}

func TestValidatedRequestEnvelopeTypedPathsDistinguishNumericKeysFromArrayIndexes(t *testing.T) {
	payload := []byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"hi"}],
		"object":{"0":"object-zero"},
		"array":["array-zero"]
	}`)
	c := newRelayValidationContext(t, "/v1/messages", payload)
	_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)

	objectRaw, kind, found, err := envelope.RawPath(
		JSONPathSegment{Key: "object"},
		JSONPathSegment{Key: "0"},
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, JSONValueString, kind)
	assert.Equal(t, `"object-zero"`, string(objectRaw))

	arrayRaw, kind, found, err := envelope.RawPath(
		JSONPathSegment{Key: "array"},
		JSONPathSegment{Index: 0, IsIndex: true},
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, JSONValueString, kind)
	assert.Equal(t, `"array-zero"`, string(arrayRaw))

	_, _, found, err = envelope.RawPath(
		JSONPathSegment{Key: "object"},
		JSONPathSegment{Index: 0, IsIndex: true},
	)
	require.NoError(t, err)
	assert.False(t, found)
	_, _, _, err = envelope.RawPath(JSONPathSegment{Key: "ambiguous", Index: 1, IsIndex: true})
	require.Error(t, err)

	inventory := envelope.Inventory()
	require.NotEmpty(t, inventory)
	var objectEntry, arrayEntry JSONInventoryEntry
	for _, entry := range inventory {
		switch entry.Pointer {
		case "/object/0":
			objectEntry = entry
		case "/array/0":
			arrayEntry = entry
		}
	}
	assert.Equal(t, []JSONPathSegment{{Key: "object"}, {Key: "0"}}, objectEntry.Segments)
	assert.Equal(t, []JSONPathSegment{{Key: "array"}, {Index: 0, IsIndex: true}}, arrayEntry.Segments)

	require.NotEmpty(t, objectEntry.Segments)
	objectEntry.Segments[0].Key = "mutated"
	for _, entry := range envelope.Inventory() {
		if entry.Pointer == "/object/0" {
			assert.Equal(t, "object", entry.Segments[0].Key)
			return
		}
	}
	t.Fatal("object numeric-key inventory entry is missing")
}

func TestValidatedRequestEnvelopeOpenAndCopySpanEnforceCallerBudget(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"value":9007199254740993,"tail":true}`)
	c := newRelayValidationContext(t, "/v1/messages", payload)
	_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)

	var valueEntry JSONInventoryEntry
	for _, entry := range envelope.Inventory() {
		if entry.Pointer == "/value" {
			valueEntry = entry
			break
		}
	}
	require.Equal(t, JSONValueNumber, valueEntry.Kind)
	spanBytes := valueEntry.End - valueEntry.Start
	require.Positive(t, spanBytes)

	reader, err := envelope.OpenSpan(valueEntry, spanBytes)
	require.NoError(t, err)
	raw, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, "9007199254740993", string(raw))

	_, err = envelope.OpenSpan(valueEntry, spanBytes-1)
	assert.ErrorIs(t, err, ErrJSONSpanTooLarge)

	var copied bytes.Buffer
	written, err := envelope.CopySpan(context.Background(), &copied, valueEntry, spanBytes)
	require.NoError(t, err)
	assert.Equal(t, spanBytes, written)
	assert.Equal(t, raw, copied.Bytes())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	written, err = envelope.CopySpan(cancelled, io.Discard, valueEntry, spanBytes)
	assert.Zero(t, written)
	assert.ErrorIs(t, err, context.Canceled)

	mutatedEntry := valueEntry
	mutatedEntry.Segments = append([]JSONPathSegment(nil), valueEntry.Segments...)
	mutatedEntry.Segments[0].Key = "other"
	_, err = envelope.OpenSpan(mutatedEntry, spanBytes)
	assert.ErrorContains(t, err, "does not belong")

	otherPayload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"value":8007199254740993,"tail":true}`)
	otherContext := newRelayValidationContext(t, "/v1/messages", otherPayload)
	_, err = GetAndValidateRequest(otherContext, types.RelayFormatClaude)
	require.NoError(t, err)
	otherEnvelope, found, err := GetValidatedRequestEnvelope(otherContext, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)
	_, err = otherEnvelope.OpenSpan(valueEntry, spanBytes)
	assert.ErrorContains(t, err, "does not belong")
}

type bytesForbiddenBodyStorage struct {
	common.BodyStorage
	bytesCalls atomic.Int32
}

func (s *bytesForbiddenBodyStorage) Bytes() ([]byte, error) {
	s.bytesCalls.Add(1)
	panic("BodyStorage.Bytes must not be called")
}

func (s *bytesForbiddenBodyStorage) IsDisk() bool { return true }

func TestValidatedRequestEnvelopeNeverCallsBodyStorageBytes(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	underlying, err := common.CreateBodyStorage(payload)
	require.NoError(t, err)
	storage := &bytesForbiddenBodyStorage{BodyStorage: underlying}
	c := newRelayValidationContext(t, "/v1/messages", payload)
	c.Set(common.KeyBodyStorage, storage)
	t.Cleanup(func() { _ = storage.Close() })

	request, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.NotNil(t, request)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)
	var stringsSeen []string
	require.NoError(t, envelope.VisitStringValues(context.Background(), func(value string) error {
		stringsSeen = append(stringsSeen, value)
		return nil
	}))
	assert.Equal(t, []string{"test-model", "user", "hi"}, stringsSeen)

	var contentEntry JSONInventoryEntry
	for _, entry := range envelope.Inventory() {
		if entry.Pointer == "/messages/0/content" {
			contentEntry = entry
			break
		}
	}
	require.Equal(t, JSONValueString, contentEntry.Kind)
	var copied bytes.Buffer
	spanBytes := contentEntry.End - contentEntry.Start
	written, err := envelope.CopySpan(context.Background(), &copied, contentEntry, spanBytes)
	require.NoError(t, err)
	assert.Equal(t, spanBytes, written)
	assert.Equal(t, `"hi"`, copied.String())
	assert.Zero(t, storage.bytesCalls.Load())
}

func TestValidatedRequestEnvelopeVisitsDecodedStringValuesInDocumentOrder(t *testing.T) {
	payload := []byte(`{
		"model":"test-model",
		"messages":[{"role":"user","content":"first"}],
		"extension":{"escaped":"\u4f60\u597d","items":["last",17,false,null]}
	}`)
	c := newRelayValidationContext(t, "/v1/messages", payload)
	_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)

	var values []string
	require.NoError(t, envelope.VisitStringValues(context.Background(), func(value string) error {
		values = append(values, value)
		return nil
	}))
	assert.Equal(t, []string{"test-model", "user", "first", "\u4f60\u597d", "last"}, values)
	assert.NotContains(t, values, "extension")
	assert.NotContains(t, values, "escaped")
}

func TestValidatedRequestEnvelopeStringVisitorHonorsCancellationAndVisitorErrors(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"first"}]}`)
	c := newRelayValidationContext(t, "/v1/messages", payload)
	_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
	require.NoError(t, err)
	envelope, found, err := GetValidatedRequestEnvelope(c, types.RelayFormatClaude)
	require.NoError(t, err)
	require.True(t, found)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err = envelope.VisitStringValues(cancelled, func(string) error {
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)

	visitorErr := errors.New("stop string visit")
	err = envelope.VisitStringValues(context.Background(), func(string) error {
		return visitorErr
	})
	assert.ErrorIs(t, err, visitorErr)
}

type mutableEnvelopeTestStorage struct {
	mutex  sync.Mutex
	data   []byte
	reader *bytes.Reader
	closed bool
}

func newMutableEnvelopeTestStorage(data []byte) *mutableEnvelopeTestStorage {
	copyData := append([]byte(nil), data...)
	return &mutableEnvelopeTestStorage{data: copyData, reader: bytes.NewReader(copyData)}
}

func (s *mutableEnvelopeTestStorage) Read(p []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return 0, common.ErrStorageClosed
	}
	return s.reader.Read(p)
}

func (s *mutableEnvelopeTestStorage) Seek(offset int64, whence int) (int64, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return 0, common.ErrStorageClosed
	}
	return s.reader.Seek(offset, whence)
}

func (s *mutableEnvelopeTestStorage) Close() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.closed = true
	return nil
}

func (s *mutableEnvelopeTestStorage) Bytes() ([]byte, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return append([]byte(nil), s.data...), nil
}

func (s *mutableEnvelopeTestStorage) Size() int64 {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return int64(len(s.data))
}

func (s *mutableEnvelopeTestStorage) IsDisk() bool { return false }

func (s *mutableEnvelopeTestStorage) NewReader() (io.ReadCloser, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return nil, common.ErrStorageClosed
	}
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

func (s *mutableEnvelopeTestStorage) mutateLastByte(value byte) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.data[len(s.data)-1] = value
}

func TestValidatedRequestEnvelopeCacheFailsClosedOnStorageChanges(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("storage identity", func(t *testing.T) {
		c := newRelayValidationContext(t, "/v1/messages", payload)
		_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
		require.NoError(t, err)
		replacement, err := common.CreateBodyStorage(payload)
		require.NoError(t, err)
		t.Cleanup(func() { _ = replacement.Close() })
		c.Set(common.KeyBodyStorage, replacement)

		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "storage")
	})

	t.Run("body fingerprint", func(t *testing.T) {
		storage := newMutableEnvelopeTestStorage(payload)
		c := newRelayValidationContext(t, "/v1/messages", payload)
		c.Set(common.KeyBodyStorage, storage)
		t.Cleanup(func() { _ = storage.Close() })
		_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
		require.NoError(t, err)
		storage.mutateLastByte(' ')

		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fingerprint")
	})

	t.Run("reported body size", func(t *testing.T) {
		underlying, err := common.CreateBodyStorage(payload)
		require.NoError(t, err)
		storage := &reportedSizeBodyStorage{BodyStorage: underlying}
		storage.reportedSize.Store(int64(len(payload)))
		c := newRelayValidationContext(t, "/v1/messages", payload)
		c.Set(common.KeyBodyStorage, storage)
		t.Cleanup(func() { _ = storage.Close() })
		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.NoError(t, err)
		storage.reportedSize.Add(1)

		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "size")
	})

	t.Run("closed storage", func(t *testing.T) {
		c := newRelayValidationContext(t, "/v1/messages", payload)
		_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
		require.NoError(t, err)
		storage, err := common.GetBodyStorage(c)
		require.NoError(t, err)
		require.NoError(t, storage.Close())

		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fingerprint")
	})

	t.Run("method", func(t *testing.T) {
		c := newRelayValidationContext(t, "/v1/messages", payload)
		_, err := GetAndValidateRequest(c, types.RelayFormatClaude)
		require.NoError(t, err)
		c.Request.Method = http.MethodPut

		_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "method")
	})
}

type reportedSizeBodyStorage struct {
	common.BodyStorage
	reportedSize atomic.Int64
}

func (s *reportedSizeBodyStorage) Size() int64 {
	return s.reportedSize.Load()
}

func TestValidatedRequestEnvelopeRejectsStorageSizeMismatch(t *testing.T) {
	payload := []byte(`{"value":0}`)
	underlying, err := common.CreateBodyStorage(payload)
	require.NoError(t, err)
	storage := &reportedSizeBodyStorage{BodyStorage: underlying}
	storage.reportedSize.Store(int64(len(payload) + 1))
	defer storage.Close()

	_, err = parseValidatedRequestEnvelope(
		context.Background(),
		storage,
		http.MethodPost,
		"/v1/messages",
		types.RelayFormatClaude,
		defaultStrictJSONLimits,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage size mismatch")
}

var errEnvelopeStorageRead = errors.New("envelope storage read failed")

type terminalErrorReader struct {
	data []byte
}

func (r *terminalErrorReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errEnvelopeStorageRead
	}
	count := copy(buffer, r.data)
	r.data = r.data[count:]
	return count, nil
}

type readFailureBodyStorage struct {
	common.BodyStorage
	payload []byte
}

func (s *readFailureBodyStorage) NewReader() (io.ReadCloser, error) {
	return io.NopCloser(&terminalErrorReader{data: append([]byte(nil), s.payload...)}), nil
}

func TestValidatedRequestEnvelopePreservesStorageReadFailure(t *testing.T) {
	payload := []byte(`{"value":0}`)
	underlying, err := common.CreateBodyStorage(payload)
	require.NoError(t, err)
	storage := &readFailureBodyStorage{BodyStorage: underlying, payload: payload}
	defer storage.Close()

	_, err = parseValidatedRequestEnvelope(
		context.Background(),
		storage,
		http.MethodPost,
		"/v1/messages",
		types.RelayFormatClaude,
		defaultStrictJSONLimits,
	)

	assert.ErrorIs(t, err, errEnvelopeStorageRead)
	_, isClientError := AsClientRequestValidationError(err)
	assert.False(t, isClientError)
}

type switchingReaderBodyStorage struct {
	common.BodyStorage
	original []byte
	changed  []byte
	readers  atomic.Int32
}

func (s *switchingReaderBodyStorage) NewReader() (io.ReadCloser, error) {
	data := s.original
	if s.readers.Add(1) >= 3 {
		data = s.changed
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestValidatedRequestEnvelopeRejectsStorageMutationDuringValidation(t *testing.T) {
	original := []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	changed := []byte(`{"model":"test-model","messages":[{"role":"user","content":"ho"}]}`)
	require.Len(t, changed, len(original))
	underlying, err := common.CreateBodyStorage(original)
	require.NoError(t, err)
	storage := &switchingReaderBodyStorage{
		BodyStorage: underlying,
		original:    original,
		changed:     changed,
	}
	c := newRelayValidationContext(t, "/v1/messages", original)
	c.Set(common.KeyBodyStorage, storage)
	t.Cleanup(func() { _ = storage.Close() })

	_, err = GetAndValidateRequest(c, types.RelayFormatClaude)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fingerprint mismatch")
}

func TestStrictJSONParserRejectsInvalidUTF8TrailingValuesAndNonObjectRoots(t *testing.T) {
	invalidUTF8Key := append([]byte(`{"`), 0xff)
	invalidUTF8Key = append(invalidUTF8Key, []byte(`":0}`)...)
	invalidUTF8Value := append([]byte(`{"value":"`), 0xff)
	invalidUTF8Value = append(invalidUTF8Value, []byte(`"}`)...)
	tests := []struct {
		name   string
		body   []byte
		ruleID string
	}{
		{name: "invalid UTF-8 key", body: invalidUTF8Key, ruleID: "json.utf8"},
		{name: "invalid UTF-8 value", body: invalidUTF8Value, ruleID: "json.utf8"},
		{name: "trailing object", body: []byte(`{} {}`), ruleID: "json.trailing_value"},
		{name: "array root", body: []byte(`[]`), ruleID: "json.root_object"},
		{name: "null root", body: []byte(`null`), ruleID: "json.root_object"},
		{name: "boolean root", body: []byte(`false`), ruleID: "json.root_object"},
		{name: "number root", body: []byte(`0`), ruleID: "json.root_object"},
		{name: "string root", body: []byte(`"value"`), ruleID: "json.root_object"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storage, err := common.CreateBodyStorage(test.body)
			require.NoError(t, err)
			defer storage.Close()

			_, err = parseValidatedRequestEnvelope(
				context.Background(),
				storage,
				http.MethodPost,
				"/v1/messages",
				types.RelayFormatClaude,
				defaultStrictJSONLimits,
			)
			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			assert.Equal(t, test.ruleID, validationErr.RuleID)
		})
	}

	storage, err := common.CreateBodyStorage([]byte(`{"value":0}`))
	require.NoError(t, err)
	defer storage.Close()
	_, err = parseValidatedRequestEnvelope(
		context.Background(),
		storage,
		http.MethodPost,
		"/v1/messages",
		types.RelayFormatClaude,
		defaultStrictJSONLimits,
	)
	require.NoError(t, err)
}

func TestStrictJSONParserEnforcesExactStructuralLimitBoundaries(t *testing.T) {
	base := strictJSONLimits{
		maxDepth:                16,
		maxNodes:                64,
		maxObjectMembers:        16,
		maxArrayElements:        16,
		maxDecodedKeyBytes:      16,
		maxTotalDecodedKeyBytes: 64,
		maxPathSegments:         128,
	}
	tests := []struct {
		name      string
		exactBody string
		overBody  string
		mutate    func(*strictJSONLimits)
		ruleID    string
	}{
		{name: "depth", exactBody: `{"a":1}`, overBody: `{"a":{"b":1}}`, mutate: func(l *strictJSONLimits) { l.maxDepth = 1 }, ruleID: "json.depth_limit"},
		{name: "nodes", exactBody: `{"a":1}`, overBody: `{"a":1,"b":2}`, mutate: func(l *strictJSONLimits) { l.maxNodes = 2 }, ruleID: "json.node_limit"},
		{name: "members", exactBody: `{"a":1}`, overBody: `{"a":1,"b":2}`, mutate: func(l *strictJSONLimits) { l.maxObjectMembers = 1 }, ruleID: "json.member_limit"},
		{name: "elements", exactBody: `{"a":[1]}`, overBody: `{"a":[1,2]}`, mutate: func(l *strictJSONLimits) { l.maxArrayElements = 1 }, ruleID: "json.element_limit"},
		{name: "decoded key bytes", exactBody: `{"aa":1}`, overBody: `{"aaa":1}`, mutate: func(l *strictJSONLimits) { l.maxDecodedKeyBytes = 2 }, ruleID: "json.key_limit"},
		{name: "total decoded key bytes", exactBody: `{"a":1,"b":2}`, overBody: `{"a":1,"bc":2}`, mutate: func(l *strictJSONLimits) { l.maxTotalDecodedKeyBytes = 2 }, ruleID: "json.key_work_limit"},
		{name: "path work", exactBody: `{"a":{"b":1}}`, overBody: `{"a":{"b":1},"c":2}`, mutate: func(l *strictJSONLimits) { l.maxPathSegments = 3 }, ruleID: "json.work_limit"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := base
			test.mutate(&limits)
			storage, err := common.CreateBodyStorage([]byte(test.exactBody))
			require.NoError(t, err)
			_, err = parseValidatedRequestEnvelope(
				context.Background(),
				storage,
				http.MethodPost,
				"/v1/messages",
				types.RelayFormatClaude,
				limits,
			)
			require.NoError(t, err)
			require.NoError(t, storage.Close())

			storage, err = common.CreateBodyStorage([]byte(test.overBody))
			require.NoError(t, err)
			defer storage.Close()
			_, err = parseValidatedRequestEnvelope(
				context.Background(),
				storage,
				http.MethodPost,
				"/v1/messages",
				types.RelayFormatClaude,
				limits,
			)
			require.Error(t, err)
			validationErr, ok := AsClientRequestValidationError(err)
			require.True(t, ok)
			assert.Equal(t, test.ruleID, validationErr.RuleID)
		})
	}
}

type cancelAfterReadBodyStorage struct {
	common.BodyStorage
	cancel         context.CancelFunc
	cancelAtReader int32
	readers        atomic.Int32
}

func (s *cancelAfterReadBodyStorage) NewReader() (io.ReadCloser, error) {
	reader, err := s.BodyStorage.NewReader()
	if err != nil {
		return nil, err
	}
	if s.readers.Add(1) != s.cancelAtReader {
		return reader, nil
	}
	return &cancelAfterReadCloser{ReadCloser: reader, cancel: s.cancel}, nil
}

type cancelAfterReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelAfterReadCloser) Read(buffer []byte) (int, error) {
	count, err := r.ReadCloser.Read(buffer)
	if count > 0 {
		r.once.Do(r.cancel)
	}
	return count, err
}

func TestStrictJSONParserHonorsCancellationDuringParse(t *testing.T) {
	payload := []byte(`{"value":"` + strings.Repeat("x", 8<<10) + `"}`)
	underlying, err := common.CreateBodyStorage(payload)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	storage := &cancelAfterReadBodyStorage{BodyStorage: underlying, cancel: cancel, cancelAtReader: 1}
	defer storage.Close()

	_, err = parseValidatedRequestEnvelope(
		ctx,
		storage,
		http.MethodPost,
		"/v1/messages",
		types.RelayFormatClaude,
		defaultStrictJSONLimits,
	)

	assert.ErrorIs(t, err, context.Canceled)
	_, isClientError := AsClientRequestValidationError(err)
	assert.False(t, isClientError)
}

func TestValidatedRequestEnvelopeHonorsCancellationDuringSemanticDecodes(t *testing.T) {
	payload := []byte(`{"model":"test-model","messages":[{"role":"user","content":"` + strings.Repeat("x", 8<<10) + `"}]}`)
	for _, test := range []struct {
		name           string
		cancelAtReader int32
	}{
		{name: "raw semantic decode", cancelAtReader: 2},
		{name: "typed DTO decode", cancelAtReader: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			underlying, err := common.CreateBodyStorage(payload)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			storage := &cancelAfterReadBodyStorage{
				BodyStorage:    underlying,
				cancel:         cancel,
				cancelAtReader: test.cancelAtReader,
			}
			c := newRelayValidationContext(t, "/v1/messages", payload)
			c.Request = c.Request.WithContext(ctx)
			c.Set(common.KeyBodyStorage, storage)
			t.Cleanup(func() { _ = storage.Close() })

			_, err = GetAndValidateRequest(c, types.RelayFormatClaude)

			assert.ErrorIs(t, err, context.Canceled)
			_, isClientError := AsClientRequestValidationError(err)
			assert.False(t, isClientError)
		})
	}
}

func TestStrictJSONParserHonorsCanceledContext(t *testing.T) {
	storage, err := common.CreateBodyStorage([]byte(`{"model":"test-model"}`))
	require.NoError(t, err)
	defer storage.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = parseValidatedRequestEnvelope(
		ctx,
		storage,
		http.MethodPost,
		"/v1/messages",
		types.RelayFormatClaude,
		defaultStrictJSONLimits,
	)

	assert.ErrorIs(t, err, context.Canceled)
}
