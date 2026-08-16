package helper

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const strictJSONContractVersion = "strict-json-envelope-v1"

type strictJSONLimits struct {
	maxDepth                int
	maxNodes                int
	maxObjectMembers        int
	maxArrayElements        int
	maxDecodedKeyBytes      int
	maxTotalDecodedKeyBytes int64
	maxPathSegments         int64
}

var defaultStrictJSONLimits = strictJSONLimits{
	maxDepth:                128,
	maxNodes:                262_144,
	maxObjectMembers:        65_536,
	maxArrayElements:        262_144,
	maxDecodedKeyBytes:      16 << 10,
	maxTotalDecodedKeyBytes: 8 << 20,
	maxPathSegments:         4 << 20,
}

// JSONValueKind records the syntactic JSON kind without decoding numbers
// through float64 or copying opaque values out of request storage.
type JSONValueKind string

const (
	JSONValueObject  JSONValueKind = "object"
	JSONValueArray   JSONValueKind = "array"
	JSONValueString  JSONValueKind = "string"
	JSONValueNumber  JSONValueKind = "number"
	JSONValueBoolean JSONValueKind = "boolean"
	JSONValueNull    JSONValueKind = "null"
)

type jsonPathSegment struct {
	key     string
	index   int
	isIndex bool
}

// JSONPathSegment preserves whether a path component is an object key or an
// array index. Pointer alone cannot distinguish an object key named "0" from
// array element zero.
type JSONPathSegment struct {
	Key     string
	Index   int
	IsIndex bool
}

type validatedJSONValue struct {
	path  []jsonPathSegment
	kind  JSONValueKind
	start int64
	end   int64
}

// JSONInventoryEntry is a defensive copy of one decoded-segment path entry.
// It is intended for invariant tests and internal contract planning only.
type JSONInventoryEntry struct {
	Pointer         string
	Segments        []JSONPathSegment
	Kind            JSONValueKind
	Start           int64
	End             int64
	bodyFingerprint string
}

// ErrJSONSpanTooLarge reports that a caller-supplied span budget is smaller
// than the selected JSON value. Callers choose a budget for their phase rather
// than materializing an arbitrary request subtree.
var ErrJSONSpanTooLarge = errors.New("validated request JSON span exceeds byte limit")

// ValidatedRequestEnvelope is the immutable request-global semantic source.
// Raw values remain in BodyStorage and are addressed by validated byte spans.
type ValidatedRequestEnvelope struct {
	method               string
	path                 string
	format               types.RelayFormat
	originalModel        string
	typedRequestType     reflect.Type
	storage              common.BodyStorage
	storageSize          int64
	bodyFingerprint      string
	inventoryFingerprint string
	contractFingerprint  string
	values               []validatedJSONValue
	topLevelOrder        []string
	topLevelFields       map[string]int
	streamPresent        bool
	streamValue          *bool
}

func (e *ValidatedRequestEnvelope) Method() string {
	if e == nil {
		return ""
	}
	return e.method
}

func (e *ValidatedRequestEnvelope) Path() string {
	if e == nil {
		return ""
	}
	return e.path
}

func (e *ValidatedRequestEnvelope) Format() types.RelayFormat {
	if e == nil {
		return ""
	}
	return e.format
}

func (e *ValidatedRequestEnvelope) OriginalModel() string {
	if e == nil {
		return ""
	}
	return e.originalModel
}

func (e *ValidatedRequestEnvelope) DecodedBytes() int64 {
	if e == nil {
		return 0
	}
	return e.storageSize
}

func (e *ValidatedRequestEnvelope) BodyFingerprint() string {
	if e == nil {
		return ""
	}
	return e.bodyFingerprint
}

func (e *ValidatedRequestEnvelope) InventoryFingerprint() string {
	if e == nil {
		return ""
	}
	return e.inventoryFingerprint
}

func (e *ValidatedRequestEnvelope) ContractFingerprint() string {
	if e == nil {
		return ""
	}
	return e.contractFingerprint
}

func (e *ValidatedRequestEnvelope) Stream() (present bool, value bool, validBoolean bool) {
	if e == nil || !e.streamPresent {
		return false, false, false
	}
	if e.streamValue == nil {
		return true, false, false
	}
	return true, *e.streamValue, true
}

func (e *ValidatedRequestEnvelope) TopLevelFieldNames() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.topLevelOrder...)
}

func (e *ValidatedRequestEnvelope) TopLevelKind(name string) (JSONValueKind, bool) {
	if e == nil {
		return "", false
	}
	index, ok := e.topLevelFields[name]
	if !ok || index < 0 || index >= len(e.values) {
		return "", false
	}
	return e.values[index].kind, true
}

func (e *ValidatedRequestEnvelope) RawTopLevelField(name string) ([]byte, bool, error) {
	if e == nil {
		return nil, false, errors.New("validated request envelope is nil")
	}
	index, ok := e.topLevelFields[name]
	if !ok {
		return nil, false, nil
	}
	if index < 0 || index >= len(e.values) {
		return nil, true, errors.New("validated request envelope inventory is corrupt")
	}
	raw, err := readBodyStorageSpan(e.storage, e.values[index].start, e.values[index].end)
	return raw, true, err
}

// RawObjectPath returns the exact JSON value span for an object-only path.
// Array traversal uses the inventory API so callers cannot confuse an object
// key containing digits with an array index.
func (e *ValidatedRequestEnvelope) RawObjectPath(path ...string) ([]byte, JSONValueKind, bool, error) {
	segments := make([]JSONPathSegment, len(path))
	for index, key := range path {
		segments[index] = JSONPathSegment{Key: key}
	}
	return e.RawPath(segments...)
}

// RawPath returns the exact JSON value for a typed object/array path.
func (e *ValidatedRequestEnvelope) RawPath(path ...JSONPathSegment) ([]byte, JSONValueKind, bool, error) {
	if e == nil {
		return nil, "", false, errors.New("validated request envelope is nil")
	}
	if !validPublicJSONPath(path) {
		return nil, "", false, errors.New("validated request envelope path is invalid")
	}
	for _, value := range e.values {
		if len(value.path) != len(path) {
			continue
		}
		matched := true
		for index, segment := range value.path {
			publicSegment := path[index]
			if segment.isIndex != publicSegment.IsIndex ||
				(segment.isIndex && (publicSegment.Index < 0 || segment.index != publicSegment.Index)) ||
				(!segment.isIndex && segment.key != publicSegment.Key) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		raw, err := readBodyStorageSpan(e.storage, value.start, value.end)
		return raw, value.kind, true, err
	}
	return nil, "", false, nil
}

// OpenSpan opens an inventory value as a bounded reader without materializing
// it. Pointer is display-only; the decoded typed path, kind, and byte offsets
// must match an entry in this envelope.
func (e *ValidatedRequestEnvelope) OpenSpan(entry JSONInventoryEntry, maxBytes int64) (io.ReadCloser, error) {
	value, err := e.resolveInventoryEntry(entry)
	if err != nil {
		return nil, err
	}
	return openBodyStorageSpan(e.storage, value.start, value.end, maxBytes)
}

// CopySpan copies an inventory value through the same byte bound as OpenSpan.
func (e *ValidatedRequestEnvelope) CopySpan(
	ctx context.Context,
	destination io.Writer,
	entry JSONInventoryEntry,
	maxBytes int64,
) (written int64, err error) {
	if destination == nil {
		return 0, errors.New("validated request JSON span destination is nil")
	}
	reader, err := e.OpenSpan(entry, maxBytes)
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := reader.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	written, err = io.Copy(destination, contextCheckingReader{ctx: ctx, reader: reader})
	if err != nil {
		return written, err
	}
	if ctx != nil && ctx.Err() != nil {
		return written, ctx.Err()
	}
	return written, nil
}

func (e *ValidatedRequestEnvelope) resolveInventoryEntry(entry JSONInventoryEntry) (validatedJSONValue, error) {
	if e == nil || e.storage == nil {
		return validatedJSONValue{}, errors.New("validated request envelope is nil")
	}
	if entry.bodyFingerprint == "" || entry.bodyFingerprint != e.bodyFingerprint ||
		!validPublicJSONPath(entry.Segments) {
		return validatedJSONValue{}, errors.New("JSON inventory entry does not belong to validated request envelope")
	}
	for _, value := range e.values {
		if value.kind != entry.Kind || value.start != entry.Start || value.end != entry.End ||
			!matchesPublicJSONPath(value.path, entry.Segments) {
			continue
		}
		return value, nil
	}
	return validatedJSONValue{}, errors.New("JSON inventory entry does not belong to validated request envelope")
}

func validPublicJSONPath(path []JSONPathSegment) bool {
	for _, segment := range path {
		if (segment.IsIndex && (segment.Index < 0 || segment.Key != "")) ||
			(!segment.IsIndex && segment.Index != 0) {
			return false
		}
	}
	return true
}

func matchesPublicJSONPath(path []jsonPathSegment, publicPath []JSONPathSegment) bool {
	if len(path) != len(publicPath) {
		return false
	}
	for index, segment := range path {
		publicSegment := publicPath[index]
		if segment.isIndex != publicSegment.IsIndex ||
			(segment.isIndex && (publicSegment.Index < 0 || segment.index != publicSegment.Index)) ||
			(!segment.isIndex && segment.key != publicSegment.Key) {
			return false
		}
	}
	return true
}

func (e *ValidatedRequestEnvelope) Inventory() []JSONInventoryEntry {
	if e == nil {
		return nil
	}
	entries := make([]JSONInventoryEntry, len(e.values))
	for index, value := range e.values {
		entries[index] = JSONInventoryEntry{
			Pointer:         jsonPointer(value.path),
			Segments:        publicJSONPath(value.path),
			Kind:            value.kind,
			Start:           value.start,
			End:             value.end,
			bodyFingerprint: e.bodyFingerprint,
		}
	}
	return entries
}

func publicJSONPath(path []jsonPathSegment) []JSONPathSegment {
	segments := make([]JSONPathSegment, len(path))
	for index, segment := range path {
		segments[index] = JSONPathSegment{
			Key:     segment.key,
			Index:   segment.index,
			IsIndex: segment.isIndex,
		}
	}
	return segments
}

// VisitStringValues decodes each JSON string value in document order without
// materializing the complete request body. Object keys and paths are not
// exposed to the visitor.
func (e *ValidatedRequestEnvelope) VisitStringValues(ctx context.Context, visit func(string) error) error {
	if e == nil || e.storage == nil {
		return errors.New("validated request envelope is nil")
	}
	if visit == nil {
		return errors.New("validated request envelope string visitor is nil")
	}
	reader, err := e.storage.NewReader()
	if err != nil {
		return err
	}
	defer reader.Close()

	checkedReader := contextCheckingReader{ctx: ctx, reader: reader}
	cursor := int64(0)
	for _, value := range e.values {
		if value.kind != JSONValueString {
			continue
		}
		if value.start < cursor || value.end < value.start || value.end > e.storageSize {
			return errors.New("validated request envelope string inventory is corrupt")
		}
		if skip := value.start - cursor; skip > 0 {
			if _, err := io.CopyN(io.Discard, checkedReader, skip); err != nil {
				return err
			}
		}
		length := value.end - value.start
		if length > int64(int(^uint(0)>>1)) {
			return errors.New("validated request envelope string is too large")
		}
		raw := make([]byte, int(length))
		if _, err := io.ReadFull(checkedReader, raw); err != nil {
			return err
		}
		cursor = value.end

		var decoded string
		if err := common.Unmarshal(raw, &decoded); err != nil {
			return errors.New("validated request envelope string cannot be decoded")
		}
		if err := visit(decoded); err != nil {
			return err
		}
	}
	return nil
}

func GetValidatedRequestEnvelope(c *gin.Context, format types.RelayFormat) (*ValidatedRequestEnvelope, bool, error) {
	cached, found, err := getCachedValidatedRequest(c)
	if err != nil || !found {
		return nil, found, err
	}
	if cached.format != format {
		return nil, true, errors.New("cached validated request envelope does not match relay format")
	}
	return cached.envelope, true, nil
}

func parseValidatedRequestEnvelope(
	ctx context.Context,
	storage common.BodyStorage,
	method string,
	path string,
	format types.RelayFormat,
	limits strictJSONLimits,
) (*ValidatedRequestEnvelope, error) {
	if storage == nil {
		return nil, errors.New("request body storage is nil")
	}
	storageSize := storage.Size()
	if storageSize < 0 {
		return nil, errors.New("request body storage size mismatch")
	}
	reader, err := storage.NewReader()
	if err != nil {
		return nil, fmt.Errorf("open request body storage: %w", err)
	}
	defer reader.Close()

	bodyHash := sha256.New()
	parser := strictJSONParser{
		ctx:    ctx,
		reader: bufio.NewReader(io.TeeReader(reader, bodyHash)),
		limits: limits,
	}
	rootIndex, err := parser.parseDocument()
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if parser.readErr != nil {
		return nil, parser.readErr
	}
	if err != nil {
		return nil, strictJSONClientError(err)
	}
	if parser.offset != storageSize || storage.Size() != storageSize {
		return nil, errors.New("request body storage size mismatch")
	}
	if rootIndex < 0 || rootIndex >= len(parser.values) || parser.values[rootIndex].kind != JSONValueObject {
		return nil, newStrictJSONClientError("json.root_object", "request body must be a JSON object")
	}

	envelope := &ValidatedRequestEnvelope{
		method:               method,
		path:                 path,
		format:               format,
		storage:              storage,
		storageSize:          storageSize,
		bodyFingerprint:      hex.EncodeToString(bodyHash.Sum(nil)),
		values:               parser.values,
		topLevelOrder:        parser.topLevelOrder,
		topLevelFields:       parser.topLevelFields,
		contractFingerprint:  contractFingerprint(format),
		inventoryFingerprint: fingerprintJSONInventory(parser.values),
	}
	if streamIndex, ok := envelope.topLevelFields["stream"]; ok {
		envelope.streamPresent = true
		if streamIndex >= 0 && streamIndex < len(envelope.values) && envelope.values[streamIndex].kind == JSONValueBoolean {
			raw, readErr := readBodyStorageSpan(storage, envelope.values[streamIndex].start, envelope.values[streamIndex].end)
			if readErr != nil {
				return nil, readErr
			}
			stream := string(raw) == "true"
			envelope.streamValue = &stream
		}
	}
	return envelope, nil
}

func (e *ValidatedRequestEnvelope) bindTypedRequest(model string, request dto.Request) error {
	if e == nil || request == nil {
		return errors.New("validated request envelope cannot bind a nil request")
	}
	if strings.TrimSpace(model) == "" {
		return errors.New("validated request envelope cannot bind an empty model")
	}
	e.originalModel = model
	e.typedRequestType = reflect.TypeOf(request)
	return nil
}

func (e *ValidatedRequestEnvelope) validateCache(
	c *gin.Context,
	format types.RelayFormat,
	method string,
	path string,
	model string,
	request dto.Request,
) error {
	if e == nil || e.storage == nil || request == nil {
		return errors.New("cached validated request envelope is invalid")
	}
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return errors.New("cached validated request envelope has no live request")
	}
	if e.method != method || method != c.Request.Method {
		return errors.New("cached validated request envelope method mismatch")
	}
	if e.path != path || path != c.Request.URL.Path {
		return errors.New("cached validated request envelope path mismatch")
	}
	if e.format != format {
		return errors.New("cached validated request envelope format mismatch")
	}
	if e.originalModel == "" || e.originalModel != model {
		return errors.New("cached validated request envelope model mismatch")
	}
	if e.typedRequestType == nil || e.typedRequestType != reflect.TypeOf(request) {
		return errors.New("cached validated request envelope DTO type mismatch")
	}
	storageValue, found := c.Get(common.KeyBodyStorage)
	storage, ok := storageValue.(common.BodyStorage)
	if !found || !ok || !sameBodyStorage(storage, e.storage) {
		return errors.New("cached validated request envelope storage mismatch")
	}
	if storage.Size() != e.storageSize {
		return errors.New("cached validated request envelope body size mismatch")
	}
	fingerprint, err := fingerprintBodyStorage(c.Request.Context(), storage)
	if err != nil {
		return fmt.Errorf("validate cached request body fingerprint: %w", err)
	}
	if fingerprint != e.bodyFingerprint {
		return errors.New("cached validated request envelope body fingerprint mismatch")
	}
	if fingerprintJSONInventory(e.values) != e.inventoryFingerprint {
		return errors.New("cached validated request envelope inventory mismatch")
	}
	if contractFingerprint(format) != e.contractFingerprint {
		return errors.New("cached validated request envelope contract mismatch")
	}
	return nil
}

func sameBodyStorage(left, right common.BodyStorage) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if leftValue.Type().Comparable() {
		return leftValue.Interface() == rightValue.Interface()
	}
	return false
}

func contractFingerprint(format types.RelayFormat) string {
	sum := sha256.Sum256([]byte(strictJSONContractVersion + "\x00" + string(format)))
	return hex.EncodeToString(sum[:])
}

func fingerprintBodyStorage(ctx context.Context, storage common.BodyStorage) (string, error) {
	if storage == nil {
		return "", errors.New("body storage is nil")
	}
	reader, err := storage.NewReader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	hasher := sha256.New()
	buffer := make([]byte, 32<<10)
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		count, readErr := reader.Read(buffer)
		if count > 0 {
			_, _ = hasher.Write(buffer[:count])
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", readErr
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

type bodyStorageReaderAt interface {
	ReadAt([]byte, int64) (int, error)
}

type spanReadCloser struct {
	io.Reader
	io.Closer
}

func openBodyStorageSpan(storage common.BodyStorage, start, end, maxBytes int64) (io.ReadCloser, error) {
	if storage == nil || start < 0 || end < start || end > storage.Size() {
		return nil, errors.New("invalid request body span")
	}
	if maxBytes < 0 {
		return nil, errors.New("request body span limit is invalid")
	}
	length := end - start
	if length > maxBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrJSONSpanTooLarge, length, maxBytes)
	}
	if readerAt, ok := storage.(bodyStorageReaderAt); ok {
		return io.NopCloser(io.NewSectionReader(readerAt, start, length)), nil
	}
	reader, err := storage.NewReader()
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(io.Discard, reader, start); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return spanReadCloser{Reader: io.LimitReader(reader, length), Closer: reader}, nil
}

func readBodyStorageSpan(storage common.BodyStorage, start, end int64) ([]byte, error) {
	if storage == nil || start < 0 || end < start || end > storage.Size() {
		return nil, errors.New("invalid request body span")
	}
	length := end - start
	if length > int64(int(^uint(0)>>1)) {
		return nil, errors.New("request body span is too large")
	}
	result := make([]byte, int(length))
	if length == 0 {
		return result, nil
	}
	if readerAt, ok := storage.(bodyStorageReaderAt); ok {
		_, err := io.ReadFull(io.NewSectionReader(readerAt, start, length), result)
		return result, err
	}
	reader, err := storage.NewReader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if _, err := io.CopyN(io.Discard, reader, start); err != nil {
		return nil, err
	}
	_, err = io.ReadFull(reader, result)
	return result, err
}

type contextCheckingReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextCheckingReader) Read(buffer []byte) (int, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	count, err := r.reader.Read(buffer)
	if r.ctx != nil && r.ctx.Err() != nil {
		return count, r.ctx.Err()
	}
	return count, err
}

func decodeBodyStorage(ctx context.Context, storage common.BodyStorage, destination any) error {
	if storage == nil {
		return errors.New("body storage is nil")
	}
	reader, err := storage.NewReader()
	if err != nil {
		return err
	}
	defer reader.Close()
	// Typed fields still decode to their declared Go types, while interface and
	// map-backed opaque subtrees retain exact JSON number lexemes for conversion.
	err = common.DecodeJsonUseNumber(contextCheckingReader{ctx: ctx, reader: reader}, destination)
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func decodeBodyStorageUseNumber(ctx context.Context, storage common.BodyStorage, destination any) error {
	if storage == nil {
		return errors.New("body storage is nil")
	}
	reader, err := storage.NewReader()
	if err != nil {
		return err
	}
	defer reader.Close()
	err = common.DecodeJsonUseNumber(contextCheckingReader{ctx: ctx, reader: reader}, destination)
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func fingerprintJSONInventory(values []validatedJSONValue) string {
	hasher := sha256.New()
	writeHashString(hasher, strictJSONContractVersion)
	var number [8]byte
	for _, value := range values {
		writeHashString(hasher, string(value.kind))
		binary.BigEndian.PutUint64(number[:], uint64(value.start))
		_, _ = hasher.Write(number[:])
		binary.BigEndian.PutUint64(number[:], uint64(value.end))
		_, _ = hasher.Write(number[:])
		binary.BigEndian.PutUint64(number[:], uint64(len(value.path)))
		_, _ = hasher.Write(number[:])
		for _, segment := range value.path {
			if segment.isIndex {
				_, _ = hasher.Write([]byte{1})
				binary.BigEndian.PutUint64(number[:], uint64(segment.index))
				_, _ = hasher.Write(number[:])
			} else {
				_, _ = hasher.Write([]byte{0})
				writeHashString(hasher, segment.key)
			}
		}
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeHashString(hasher hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write([]byte(value))
}

func jsonPointer(path []jsonPathSegment) string {
	if len(path) == 0 {
		return ""
	}
	var builder strings.Builder
	for _, segment := range path {
		builder.WriteByte('/')
		if segment.isIndex {
			builder.WriteString(strconv.Itoa(segment.index))
			continue
		}
		key := strings.ReplaceAll(segment.key, "~", "~0")
		key = strings.ReplaceAll(key, "/", "~1")
		builder.WriteString(key)
	}
	return builder.String()
}

type strictJSONRuleError struct {
	ruleID  string
	message string
	cause   error
}

func (e *strictJSONRuleError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func (e *strictJSONRuleError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func newStrictJSONRuleError(ruleID, message string) error {
	return &strictJSONRuleError{ruleID: ruleID, message: message}
}

func strictJSONClientError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ruleErr *strictJSONRuleError
	if errors.As(err, &ruleErr) {
		return newStrictJSONClientError(ruleErr.ruleID, ruleErr.message)
	}
	// Reader/storage failures are gateway I/O failures, not evidence that the
	// client's JSON is malformed. Preserve the cause so the middleware can
	// render its provider-neutral read-failure response.
	return err
}

func newStrictJSONClientError(ruleID, message string) error {
	return &ClientRequestValidationError{
		StatusCode: httpStatusBadRequest,
		Message:    message,
		RuleID:     ruleID,
	}
}

const httpStatusBadRequest = 400

type strictJSONParser struct {
	ctx                  context.Context
	reader               *bufio.Reader
	limits               strictJSONLimits
	offset               int64
	readSinceContextPoll int
	readErr              error
	nodes                int
	totalKeyBytes        int64
	pathSegments         int64
	values               []validatedJSONValue
	topLevelOrder        []string
	topLevelFields       map[string]int
}

func (p *strictJSONParser) parseDocument() (int, error) {
	if p.reader == nil {
		return -1, newStrictJSONRuleError("json.empty", "request body must be a JSON object")
	}
	if p.ctx != nil {
		select {
		case <-p.ctx.Done():
			return -1, p.ctx.Err()
		default:
		}
	}
	if p.limits.maxDepth <= 0 || p.limits.maxNodes <= 0 {
		return -1, newStrictJSONRuleError("json.resource_limit", "request body exceeds JSON structural limits")
	}
	if err := p.skipWhitespace(); err != nil {
		if errors.Is(err, io.EOF) {
			return -1, newStrictJSONRuleError("json.empty", "request body must be a JSON object")
		}
		return -1, err
	}
	root, err := p.parseValue(nil, 0)
	if err != nil {
		return -1, err
	}
	if err := p.skipWhitespace(); err != nil && !errors.Is(err, io.EOF) {
		return -1, err
	}
	if _, err := p.peekByte(); err == nil {
		return -1, newStrictJSONRuleError("json.trailing_value", "request body must contain exactly one valid JSON object")
	} else if !errors.Is(err, io.EOF) {
		return -1, err
	}
	return root, nil
}

func (p *strictJSONParser) parseValue(path []jsonPathSegment, depth int) (int, error) {
	if depth > p.limits.maxDepth {
		return -1, newStrictJSONRuleError("json.depth_limit", "request body exceeds JSON structural limits")
	}
	p.nodes++
	if p.nodes > p.limits.maxNodes {
		return -1, newStrictJSONRuleError("json.node_limit", "request body exceeds JSON structural limits")
	}
	p.pathSegments += int64(len(path))
	if p.pathSegments > p.limits.maxPathSegments {
		return -1, newStrictJSONRuleError("json.work_limit", "request body exceeds JSON structural limits")
	}
	if err := p.skipWhitespace(); err != nil {
		return -1, newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	start := p.offset
	first, err := p.peekByte()
	if err != nil {
		return -1, newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	var kind JSONValueKind
	switch first {
	case '{':
		kind = JSONValueObject
		if err := p.parseObject(path, depth); err != nil {
			return -1, err
		}
	case '[':
		kind = JSONValueArray
		if err := p.parseArray(path, depth); err != nil {
			return -1, err
		}
	case '"':
		kind = JSONValueString
		if _, err := p.parseString(false); err != nil {
			return -1, err
		}
	case 't':
		kind = JSONValueBoolean
		if err := p.consumeLiteral("true"); err != nil {
			return -1, err
		}
	case 'f':
		kind = JSONValueBoolean
		if err := p.consumeLiteral("false"); err != nil {
			return -1, err
		}
	case 'n':
		kind = JSONValueNull
		if err := p.consumeLiteral("null"); err != nil {
			return -1, err
		}
	default:
		kind = JSONValueNumber
		if err := p.parseNumber(); err != nil {
			return -1, err
		}
	}
	value := validatedJSONValue{
		path:  append([]jsonPathSegment(nil), path...),
		kind:  kind,
		start: start,
		end:   p.offset,
	}
	p.values = append(p.values, value)
	return len(p.values) - 1, nil
}

func (p *strictJSONParser) parseObject(path []jsonPathSegment, depth int) error {
	if _, err := p.expectByte('{'); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if next, err := p.peekByte(); err == nil && next == '}' {
		_, _ = p.readByte()
		return nil
	}
	seen := make(map[string]struct{})
	members := 0
	for {
		members++
		if members > p.limits.maxObjectMembers {
			return newStrictJSONRuleError("json.member_limit", "request body exceeds JSON structural limits")
		}
		key, err := p.parseString(true)
		if err != nil {
			return err
		}
		if len(key) > p.limits.maxDecodedKeyBytes {
			return newStrictJSONRuleError("json.key_limit", "request body exceeds JSON structural limits")
		}
		p.totalKeyBytes += int64(len(key))
		if p.totalKeyBytes > p.limits.maxTotalDecodedKeyBytes {
			return newStrictJSONRuleError("json.key_work_limit", "request body exceeds JSON structural limits")
		}
		if _, duplicate := seen[key]; duplicate {
			return newStrictJSONRuleError("json.duplicate_key", "request body contains duplicate JSON object members")
		}
		seen[key] = struct{}{}
		if err := p.skipWhitespace(); err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		if _, err := p.expectByte(':'); err != nil {
			return err
		}
		childPath := append(path, jsonPathSegment{key: key})
		childIndex, err := p.parseValue(childPath, depth+1)
		if err != nil {
			return err
		}
		if len(path) == 0 {
			if p.topLevelFields == nil {
				p.topLevelFields = make(map[string]int)
			}
			p.topLevelFields[key] = childIndex
			p.topLevelOrder = append(p.topLevelOrder, key)
		}
		if err := p.skipWhitespace(); err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		next, err := p.readByte()
		if err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		switch next {
		case '}':
			return nil
		case ',':
			if err := p.skipWhitespace(); err != nil {
				return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
			}
		default:
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	}
}

func (p *strictJSONParser) parseArray(path []jsonPathSegment, depth int) error {
	if _, err := p.expectByte('['); err != nil {
		return err
	}
	if err := p.skipWhitespace(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if next, err := p.peekByte(); err == nil && next == ']' {
		_, _ = p.readByte()
		return nil
	}
	for index := 0; ; index++ {
		if index >= p.limits.maxArrayElements {
			return newStrictJSONRuleError("json.element_limit", "request body exceeds JSON structural limits")
		}
		childPath := append(path, jsonPathSegment{index: index, isIndex: true})
		if _, err := p.parseValue(childPath, depth+1); err != nil {
			return err
		}
		if err := p.skipWhitespace(); err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		next, err := p.readByte()
		if err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		switch next {
		case ']':
			return nil
		case ',':
			if err := p.skipWhitespace(); err != nil {
				return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
			}
		default:
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	}
}

func (p *strictJSONParser) parseString(decode bool) (string, error) {
	if _, err := p.expectByte('"'); err != nil {
		return "", err
	}
	var decoded []byte
	if decode {
		decoded = make([]byte, 0, 32)
	}
	for {
		value, err := p.readByte()
		if err != nil {
			return "", newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		switch {
		case value == '"':
			return string(decoded), nil
		case value == '\\':
			escaped, err := p.readByte()
			if err != nil {
				return "", newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
			}
			switch escaped {
			case '"', '\\', '/':
				if decode {
					decoded = append(decoded, escaped)
				}
			case 'b':
				if decode {
					decoded = append(decoded, '\b')
				}
			case 'f':
				if decode {
					decoded = append(decoded, '\f')
				}
			case 'n':
				if decode {
					decoded = append(decoded, '\n')
				}
			case 'r':
				if decode {
					decoded = append(decoded, '\r')
				}
			case 't':
				if decode {
					decoded = append(decoded, '\t')
				}
			case 'u':
				runeValue, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				if decode {
					decoded = utf8.AppendRune(decoded, runeValue)
				}
			default:
				return "", newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
			}
		case value < 0x20:
			return "", newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		case value < utf8.RuneSelf:
			if decode {
				decoded = append(decoded, value)
			}
		default:
			sequence, err := p.readUTF8Sequence(value)
			if err != nil {
				return "", err
			}
			if decode {
				decoded = append(decoded, sequence...)
			}
		}
		if decode && len(decoded) > p.limits.maxDecodedKeyBytes {
			return "", newStrictJSONRuleError("json.key_limit", "request body exceeds JSON structural limits")
		}
	}
}

func (p *strictJSONParser) parseUnicodeEscape() (rune, error) {
	first, err := p.readHexQuad()
	if err != nil {
		return 0, err
	}
	switch {
	case first >= 0xD800 && first <= 0xDBFF:
		backslash, err := p.readByte()
		if err != nil || backslash != '\\' {
			return 0, newStrictJSONRuleError("json.unpaired_surrogate", "request body contains invalid Unicode escapes")
		}
		marker, err := p.readByte()
		if err != nil || marker != 'u' {
			return 0, newStrictJSONRuleError("json.unpaired_surrogate", "request body contains invalid Unicode escapes")
		}
		second, err := p.readHexQuad()
		if err != nil {
			return 0, err
		}
		if second < 0xDC00 || second > 0xDFFF {
			return 0, newStrictJSONRuleError("json.unpaired_surrogate", "request body contains invalid Unicode escapes")
		}
		return rune(0x10000 + (int(first)-0xD800)<<10 + int(second) - 0xDC00), nil
	case first >= 0xDC00 && first <= 0xDFFF:
		return 0, newStrictJSONRuleError("json.unpaired_surrogate", "request body contains invalid Unicode escapes")
	default:
		return rune(first), nil
	}
}

func (p *strictJSONParser) readHexQuad() (uint16, error) {
	var value uint16
	for index := 0; index < 4; index++ {
		digit, err := p.readByte()
		if err != nil {
			return 0, newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit-'A') + 10
		default:
			return 0, newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	}
	return value, nil
}

func (p *strictJSONParser) readUTF8Sequence(first byte) ([]byte, error) {
	sequence := []byte{first}
	continuations := 0
	secondMin, secondMax := byte(0x80), byte(0xBF)
	switch {
	case first >= 0xC2 && first <= 0xDF:
		continuations = 1
	case first == 0xE0:
		continuations, secondMin = 2, 0xA0
	case first >= 0xE1 && first <= 0xEC:
		continuations = 2
	case first == 0xED:
		continuations, secondMax = 2, 0x9F
	case first >= 0xEE && first <= 0xEF:
		continuations = 2
	case first == 0xF0:
		continuations, secondMin = 3, 0x90
	case first >= 0xF1 && first <= 0xF3:
		continuations = 3
	case first == 0xF4:
		continuations, secondMax = 3, 0x8F
	default:
		return nil, newStrictJSONRuleError("json.utf8", "request body must contain valid UTF-8")
	}
	for index := 0; index < continuations; index++ {
		value, err := p.readByte()
		if err != nil {
			return nil, newStrictJSONRuleError("json.utf8", "request body must contain valid UTF-8")
		}
		minimum, maximum := byte(0x80), byte(0xBF)
		if index == 0 {
			minimum, maximum = secondMin, secondMax
		}
		if value < minimum || value > maximum {
			return nil, newStrictJSONRuleError("json.utf8", "request body must contain valid UTF-8")
		}
		sequence = append(sequence, value)
	}
	return sequence, nil
}

func (p *strictJSONParser) consumeLiteral(literal string) error {
	for index := 0; index < len(literal); index++ {
		value, err := p.readByte()
		if err != nil || value != literal[index] {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	}
	return nil
}

func (p *strictJSONParser) parseNumber() error {
	next, err := p.peekByte()
	if err != nil {
		return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	if next == '-' {
		_, _ = p.readByte()
		next, err = p.peekByte()
		if err != nil {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	}
	switch {
	case next == '0':
		_, _ = p.readByte()
		if following, peekErr := p.peekByte(); peekErr == nil && following >= '0' && following <= '9' {
			return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
		}
	case next >= '1' && next <= '9':
		for {
			value, peekErr := p.peekByte()
			if peekErr != nil || value < '0' || value > '9' {
				break
			}
			_, _ = p.readByte()
		}
	default:
		return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	if value, peekErr := p.peekByte(); peekErr == nil && value == '.' {
		_, _ = p.readByte()
		if err := p.consumeNumberDigits(); err != nil {
			return err
		}
	}
	if value, peekErr := p.peekByte(); peekErr == nil && (value == 'e' || value == 'E') {
		_, _ = p.readByte()
		if sign, signErr := p.peekByte(); signErr == nil && (sign == '+' || sign == '-') {
			_, _ = p.readByte()
		}
		if err := p.consumeNumberDigits(); err != nil {
			return err
		}
	}
	return nil
}

func (p *strictJSONParser) consumeNumberDigits() error {
	count := 0
	for {
		value, err := p.peekByte()
		if err != nil || value < '0' || value > '9' {
			break
		}
		_, _ = p.readByte()
		count++
	}
	if count == 0 {
		return newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	return nil
}

func (p *strictJSONParser) skipWhitespace() error {
	for {
		value, err := p.peekByte()
		if err != nil {
			return err
		}
		switch value {
		case ' ', '\t', '\r', '\n':
			_, _ = p.readByte()
		default:
			return nil
		}
	}
}

func (p *strictJSONParser) expectByte(expected byte) (byte, error) {
	value, err := p.readByte()
	if err != nil || value != expected {
		return 0, newStrictJSONRuleError("json.syntax", "request body must contain exactly one valid JSON object")
	}
	return value, nil
}

func (p *strictJSONParser) peekByte() (byte, error) {
	value, err := p.reader.Peek(1)
	if err != nil {
		if !errors.Is(err, io.EOF) {
			p.readErr = err
		}
		return 0, err
	}
	return value[0], nil
}

func (p *strictJSONParser) readByte() (byte, error) {
	if p.ctx != nil && p.readSinceContextPoll >= 4096 {
		select {
		case <-p.ctx.Done():
			return 0, p.ctx.Err()
		default:
		}
		p.readSinceContextPoll = 0
	}
	value, err := p.reader.ReadByte()
	if err != nil {
		if !errors.Is(err, io.EOF) {
			p.readErr = err
		}
		return 0, err
	}
	p.offset++
	p.readSinceContextPoll++
	return value, nil
}
