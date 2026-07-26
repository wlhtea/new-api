package seedance

import (
	"bufio"
	"errors"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	errInvalidVideoJSON               = errors.New("invalid video JSON")
	errInvalidJSONStringEscape        = errors.New("invalid JSON string escape")
	errVideoBase64Missing             = errors.New("video_base64 is missing")
	errDuplicateVideoBase64           = errors.New("duplicate video_base64")
	errVideoBase64NotString           = errors.New("video_base64 must be a string")
	errVideoBase64UnicodeEscape       = errors.New("video_base64 must not use Unicode escapes")
	errJSONNestingTooDeep             = errors.New("JSON nesting depth exceeds parser limit")
	errInvalidContentBusinessEnvelope = errors.New(
		"invalid upstream content business envelope",
	)
	errContentBusinessFailure = errors.New("upstream content business failure")
)

type jsonScanState uint8

const (
	scanValue jsonScanState = iota
	scanObjectKey
	scanObjectColon
	scanObjectComma
	scanArrayValue
	scanArrayComma
)

// maxJSONNestingDepth is a general JSON grammar-complexity guard aligned with
// encoding/json's parser depth. It is not a Seed Dance response-size limit.
const maxJSONNestingDepth = 10_000

type jsonFrame struct {
	object     bool
	state      jsonScanState
	afterComma bool
}

type jsonStringSink func([]byte) error

type contentRootField uint8

const (
	contentRootFieldOther contentRootField = iota
	contentRootFieldVideoBase64
	contentRootFieldSuccess
	contentRootFieldStatus
	contentRootFieldErrCode
)

type contentBusinessState struct {
	successSeen    bool
	success        bool
	statusSeen     bool
	statusFailed   bool
	errCodeSeen    bool
	errCodeNonZero bool
}

type jsonExtractor struct {
	src      *bufio.Reader
	redacted *bufio.Writer
	encoded  *bufio.Writer

	videoBase64Seen    bool
	videoBase64Present bool
	pendingRootField   contentRootField
	business           contentBusinessState
	stringByte         [1]byte
}

type rootFieldMatcher struct {
	index              int
	videoBase64Matches bool
	successMatches     bool
	statusMatches      bool
	errCodeMatches     bool
}

func newRootFieldMatcher() *rootFieldMatcher {
	return &rootFieldMatcher{
		videoBase64Matches: true,
		successMatches:     true,
		statusMatches:      true,
		errCodeMatches:     true,
	}
}

func (m *rootFieldMatcher) Write(decoded []byte) error {
	for _, value := range decoded {
		m.videoBase64Matches = rootFieldByteMatches(
			m.videoBase64Matches,
			"video_base64",
			m.index,
			value,
		)
		m.successMatches = rootFieldByteMatches(
			m.successMatches,
			"success",
			m.index,
			value,
		)
		m.statusMatches = rootFieldByteMatches(
			m.statusMatches,
			"status",
			m.index,
			value,
		)
		m.errCodeMatches = rootFieldByteMatches(
			m.errCodeMatches,
			"errCode",
			m.index,
			value,
		)
		m.index++
	}
	return nil
}

func rootFieldByteMatches(
	matches bool,
	target string,
	index int,
	value byte,
) bool {
	return matches && index < len(target) && value == target[index]
}

func (m *rootFieldMatcher) Field() contentRootField {
	switch {
	case m.videoBase64Matches && m.index == len("video_base64"):
		return contentRootFieldVideoBase64
	case m.successMatches && m.index == len("success"):
		return contentRootFieldSuccess
	case m.statusMatches && m.index == len("status"):
		return contentRootFieldStatus
	case m.errCodeMatches && m.index == len("errCode"):
		return contentRootFieldErrCode
	default:
		return contentRootFieldOther
	}
}

type asciiFoldMatcher struct {
	target string
	index  int
	match  bool
}

func newASCIIFoldMatcher(target string) *asciiFoldMatcher {
	return &asciiFoldMatcher{target: target, match: true}
}

func (m *asciiFoldMatcher) Write(value []byte) error {
	for _, character := range value {
		if m.index >= len(m.target) ||
			toLowerASCII(character) != toLowerASCII(m.target[m.index]) {
			m.match = false
		}
		m.index++
	}
	return nil
}

func (m *asciiFoldMatcher) Matches() bool {
	return m.match && m.index == len(m.target)
}

type zeroErrCodeStringState uint8

const (
	zeroErrCodeLeading zeroErrCodeStringState = iota
	zeroErrCodeSign
	zeroErrCodeInteger
	zeroErrCodeFractionStart
	zeroErrCodeFraction
	zeroErrCodeExponentStart
	zeroErrCodeExponentSign
	zeroErrCodeExponentDigits
	zeroErrCodeTrailing
	zeroErrCodeNonZero
)

type zeroErrCodeStringTracker struct {
	state zeroErrCodeStringState
}

func (t *zeroErrCodeStringTracker) Write(value []byte) error {
	for _, character := range value {
		t.consume(character)
	}
	return nil
}

func (t *zeroErrCodeStringTracker) consume(character byte) {
	switch t.state {
	case zeroErrCodeLeading:
		switch {
		case isErrCodeWhitespace(character):
		case character == '+' || character == '-':
			t.state = zeroErrCodeSign
		case character == '0':
			t.state = zeroErrCodeInteger
		default:
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeSign:
		if character == '0' {
			t.state = zeroErrCodeInteger
		} else {
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeInteger:
		switch {
		case character == '0':
		case character == '.':
			t.state = zeroErrCodeFractionStart
		case character == 'e' || character == 'E':
			t.state = zeroErrCodeExponentStart
		case isErrCodeWhitespace(character):
			t.state = zeroErrCodeTrailing
		default:
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeFractionStart:
		if character == '0' {
			t.state = zeroErrCodeFraction
		} else {
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeFraction:
		switch {
		case character == '0':
		case character == 'e' || character == 'E':
			t.state = zeroErrCodeExponentStart
		case isErrCodeWhitespace(character):
			t.state = zeroErrCodeTrailing
		default:
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeExponentStart:
		switch {
		case character == '+' || character == '-':
			t.state = zeroErrCodeExponentSign
		case isJSONDigit(character):
			t.state = zeroErrCodeExponentDigits
		default:
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeExponentSign:
		if isJSONDigit(character) {
			t.state = zeroErrCodeExponentDigits
		} else {
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeExponentDigits:
		if isJSONDigit(character) {
			return
		}
		if isErrCodeWhitespace(character) {
			t.state = zeroErrCodeTrailing
			return
		}
		t.state = zeroErrCodeNonZero
	case zeroErrCodeTrailing:
		if !isErrCodeWhitespace(character) {
			t.state = zeroErrCodeNonZero
		}
	case zeroErrCodeNonZero:
	}
}

func (t *zeroErrCodeStringTracker) EmptyOrZero() bool {
	switch t.state {
	case zeroErrCodeLeading,
		zeroErrCodeInteger,
		zeroErrCodeFraction,
		zeroErrCodeExponentDigits,
		zeroErrCodeTrailing:
		return true
	default:
		return false
	}
}

type zeroJSONNumberTracker struct {
	inExponent      bool
	nonZeroMantissa bool
}

func (t *zeroJSONNumberTracker) Write(value []byte) error {
	for _, character := range value {
		if character == 'e' || character == 'E' {
			t.inExponent = true
			continue
		}
		if !t.inExponent && character >= '1' && character <= '9' {
			t.nonZeroMantissa = true
		}
	}
	return nil
}

func (t *zeroJSONNumberTracker) IsZero() bool {
	return !t.nonZeroMantissa
}

func validateContentBusinessState(state contentBusinessState) error {
	if !state.successSeen {
		return errInvalidContentBusinessEnvelope
	}
	if !state.success || state.statusFailed || state.errCodeNonZero {
		return errContentBusinessFailure
	}
	return nil
}

// extractVideoBase64JSON consumes one JSON root object, writes a safe redacted
// copy, and streams the decoded root-level video_base64 JSON string to encoded.
func extractVideoBase64JSON(
	src io.Reader,
	redacted io.Writer,
	encoded io.Writer,
) error {
	_, err := extractVideoBase64JSONWithBusiness(src, redacted, encoded)
	return err
}

func extractVideoBase64JSONWithBusiness(
	src io.Reader,
	redacted io.Writer,
	encoded io.Writer,
) (contentBusinessState, error) {
	if src == nil || redacted == nil || encoded == nil {
		return contentBusinessState{}, errInvalidVideoJSON
	}

	extractor := &jsonExtractor{
		src:      bufio.NewReaderSize(src, 32*1024),
		redacted: bufio.NewWriterSize(redacted, 32*1024),
		encoded:  bufio.NewWriterSize(encoded, 32*1024),
	}
	if err := extractor.scan(); err != nil {
		return contentBusinessState{}, err
	}
	if err := extractor.encoded.Flush(); err != nil {
		return contentBusinessState{}, err
	}
	if err := extractor.redacted.Flush(); err != nil {
		return contentBusinessState{}, err
	}
	return extractor.business, nil
}

func (p *jsonExtractor) scan() error {
	first, err := p.nextNonSpace(true)
	if err != nil {
		return p.invalidRead(err)
	}
	if first != '{' {
		return errInvalidVideoJSON
	}

	frames := []jsonFrame{{object: true, state: scanObjectKey}}
	for len(frames) > 0 {
		index := len(frames) - 1
		frame := frames[index]

		switch frame.state {
		case scanObjectKey:
			keyStart, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			if keyStart == '}' {
				if frames[index].afterComma {
					return errInvalidVideoJSON
				}
				p.completeContainer(&frames)
				continue
			}
			if keyStart != '"' {
				return errInvalidVideoJSON
			}

			var matcher *rootFieldMatcher
			var keySink jsonStringSink
			if len(frames) == 1 {
				matcher = newRootFieldMatcher()
				keySink = matcher.Write
			}
			if err := p.scanJSONString(true, keySink, false); err != nil {
				return err
			}

			if matcher != nil {
				p.pendingRootField = matcher.Field()
				switch p.pendingRootField {
				case contentRootFieldVideoBase64:
					if p.videoBase64Seen {
						return errDuplicateVideoBase64
					}
					p.videoBase64Seen = true
				case contentRootFieldSuccess:
					if p.business.successSeen {
						return errInvalidContentBusinessEnvelope
					}
					p.business.successSeen = true
				case contentRootFieldStatus:
					if p.business.statusSeen {
						return errInvalidContentBusinessEnvelope
					}
					p.business.statusSeen = true
				case contentRootFieldErrCode:
					if p.business.errCodeSeen {
						return errInvalidContentBusinessEnvelope
					}
					p.business.errCodeSeen = true
				}
			}
			frames[index].state = scanObjectColon
			frames[index].afterComma = false

		case scanObjectColon:
			colon, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			if colon != ':' {
				return errInvalidVideoJSON
			}
			frames[index].state = scanValue

		case scanValue:
			field := contentRootFieldOther
			if len(frames) == 1 {
				field = p.pendingRootField
			}
			valueStart, err := p.nextNonSpace(field != contentRootFieldVideoBase64)
			if err != nil {
				return p.invalidRead(err)
			}
			if err := p.scanValueFromStart(
				&frames,
				valueStart,
				field,
			); err != nil {
				return err
			}

		case scanObjectComma:
			separator, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			switch separator {
			case ',':
				frames[index].state = scanObjectKey
				frames[index].afterComma = true
			case '}':
				p.completeContainer(&frames)
			default:
				return errInvalidVideoJSON
			}

		case scanArrayValue:
			valueStart, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			if valueStart == ']' {
				if frames[index].afterComma {
					return errInvalidVideoJSON
				}
				p.completeContainer(&frames)
				continue
			}
			if err := p.scanValueFromStart(
				&frames,
				valueStart,
				contentRootFieldOther,
			); err != nil {
				return err
			}

		case scanArrayComma:
			separator, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			switch separator {
			case ',':
				frames[index].state = scanArrayValue
				frames[index].afterComma = true
			case ']':
				p.completeContainer(&frames)
			default:
				return errInvalidVideoJSON
			}

		default:
			return errInvalidVideoJSON
		}
	}

	if err := p.consumeTrailingWhitespace(); err != nil {
		return err
	}
	if !p.videoBase64Present {
		return errVideoBase64Missing
	}
	return nil
}

func (p *jsonExtractor) scanValueFromStart(
	frames *[]jsonFrame,
	start byte,
	field contentRootField,
) error {
	switch field {
	case contentRootFieldVideoBase64:
		if start != '"' {
			return errVideoBase64NotString
		}
		if err := p.writeRedacted([]byte(`"[redacted]"`)); err != nil {
			return err
		}
		if err := p.scanJSONString(false, p.writeEncoded, true); err != nil {
			return err
		}
		p.videoBase64Present = true
		p.pendingRootField = contentRootFieldOther
		p.completeScalar(*frames)
		return nil
	case contentRootFieldSuccess:
		switch start {
		case 't':
			if err := p.scanLiteral("rue"); err != nil {
				return err
			}
			p.business.success = true
		case 'f':
			if err := p.scanLiteral("alse"); err != nil {
				return err
			}
			p.business.success = false
		default:
			return errInvalidContentBusinessEnvelope
		}
		p.pendingRootField = contentRootFieldOther
		p.completeScalar(*frames)
		return nil
	case contentRootFieldStatus:
		if start != '"' {
			return errInvalidContentBusinessEnvelope
		}
		matcher := newASCIIFoldMatcher("failed")
		if err := p.scanJSONString(true, matcher.Write, false); err != nil {
			return err
		}
		p.business.statusFailed = matcher.Matches()
		p.pendingRootField = contentRootFieldOther
		p.completeScalar(*frames)
		return nil
	case contentRootFieldErrCode:
		switch {
		case start == '"':
			tracker := &zeroErrCodeStringTracker{}
			if err := p.scanJSONString(true, tracker.Write, false); err != nil {
				return err
			}
			p.business.errCodeNonZero = !tracker.EmptyOrZero()
		case start == '-' || isJSONDigit(start):
			tracker := &zeroJSONNumberTracker{}
			if err := p.scanNumber(start, tracker.Write); err != nil {
				return err
			}
			p.business.errCodeNonZero = !tracker.IsZero()
		default:
			return errInvalidContentBusinessEnvelope
		}
		p.pendingRootField = contentRootFieldOther
		p.completeScalar(*frames)
		return nil
	}

	switch start {
	case '"':
		if err := p.scanJSONString(true, nil, false); err != nil {
			return err
		}
		p.completeScalar(*frames)
		return nil
	case '{':
		return p.pushFrame(frames, true, scanObjectKey)
	case '[':
		return p.pushFrame(frames, false, scanArrayValue)
	case 't':
		if err := p.scanLiteral("rue"); err != nil {
			return err
		}
	case 'f':
		if err := p.scanLiteral("alse"); err != nil {
			return err
		}
	case 'n':
		if err := p.scanLiteral("ull"); err != nil {
			return err
		}
	default:
		if err := p.scanNumber(start, nil); err != nil {
			return err
		}
	}

	p.completeScalar(*frames)
	return nil
}

func (p *jsonExtractor) completeScalar(frames []jsonFrame) {
	index := len(frames) - 1
	if frames[index].object {
		frames[index].state = scanObjectComma
		return
	}
	frames[index].state = scanArrayComma
}

func (p *jsonExtractor) pushFrame(
	frames *[]jsonFrame,
	object bool,
	state jsonScanState,
) error {
	if len(*frames) >= maxJSONNestingDepth {
		return errJSONNestingTooDeep
	}
	*frames = append(*frames, jsonFrame{object: object, state: state})
	return nil
}

func (p *jsonExtractor) completeContainer(frames *[]jsonFrame) {
	*frames = (*frames)[:len(*frames)-1]
	if len(*frames) > 0 {
		p.completeScalar(*frames)
	}
}

func (p *jsonExtractor) scanLiteral(rest string) error {
	for index := 0; index < len(rest); index++ {
		value, err := p.readAndCopy()
		if err != nil {
			return p.invalidRead(err)
		}
		if value != rest[index] {
			return errInvalidVideoJSON
		}
	}
	return p.requireValueDelimiter()
}

func (p *jsonExtractor) scanNumber(first byte, sink jsonStringSink) error {
	if err := p.emitJSONStringByte(sink, first); err != nil {
		return err
	}
	current := first
	if current == '-' {
		value, err := p.readAndCopyTo(sink)
		if err != nil {
			return p.invalidRead(err)
		}
		current = value
	}

	switch {
	case current == '0':
		if next, err := p.peek(); err == nil && isJSONDigit(next) {
			return errInvalidVideoJSON
		} else if err != nil && err != io.EOF {
			return err
		}
	case current >= '1' && current <= '9':
		for {
			next, err := p.peek()
			if err == io.EOF || !isJSONDigit(next) {
				if err != nil && err != io.EOF {
					return err
				}
				break
			}
			if _, err := p.readAndCopyTo(sink); err != nil {
				return p.invalidRead(err)
			}
		}
	default:
		return errInvalidVideoJSON
	}

	if next, err := p.peek(); err == nil && next == '.' {
		if _, err := p.readAndCopyTo(sink); err != nil {
			return p.invalidRead(err)
		}
		fraction, err := p.readAndCopyTo(sink)
		if err != nil {
			return p.invalidRead(err)
		}
		if !isJSONDigit(fraction) {
			return errInvalidVideoJSON
		}
		for {
			next, err := p.peek()
			if err == io.EOF || !isJSONDigit(next) {
				if err != nil && err != io.EOF {
					return err
				}
				break
			}
			if _, err := p.readAndCopyTo(sink); err != nil {
				return p.invalidRead(err)
			}
		}
	} else if err != nil && err != io.EOF {
		return err
	}

	if next, err := p.peek(); err == nil && (next == 'e' || next == 'E') {
		if _, err := p.readAndCopyTo(sink); err != nil {
			return p.invalidRead(err)
		}
		if sign, err := p.peek(); err == nil && (sign == '+' || sign == '-') {
			if _, err := p.readAndCopyTo(sink); err != nil {
				return p.invalidRead(err)
			}
		} else if err != nil && err != io.EOF {
			return err
		}
		exponent, err := p.readAndCopyTo(sink)
		if err != nil {
			return p.invalidRead(err)
		}
		if !isJSONDigit(exponent) {
			return errInvalidVideoJSON
		}
		for {
			next, err := p.peek()
			if err == io.EOF || !isJSONDigit(next) {
				if err != nil && err != io.EOF {
					return err
				}
				break
			}
			if _, err := p.readAndCopyTo(sink); err != nil {
				return p.invalidRead(err)
			}
		}
	} else if err != nil && err != io.EOF {
		return err
	}

	return p.requireValueDelimiter()
}

func (p *jsonExtractor) scanJSONString(
	copyRaw bool,
	sink jsonStringSink,
	rejectUnicodeEscape bool,
) error {
	for {
		value, err := p.readStringByte(copyRaw)
		if err != nil {
			return p.invalidRead(err)
		}

		switch value {
		case '"':
			return nil
		case '\\':
			escaped, err := p.readStringByte(copyRaw)
			if err != nil {
				return p.invalidRead(err)
			}
			switch escaped {
			case '"', '\\', '/':
				if err := p.emitJSONStringByte(sink, escaped); err != nil {
					return err
				}
			case 'b':
				if err := p.emitJSONStringByte(sink, '\b'); err != nil {
					return err
				}
			case 'f':
				if err := p.emitJSONStringByte(sink, '\f'); err != nil {
					return err
				}
			case 'n':
				if err := p.emitJSONStringByte(sink, '\n'); err != nil {
					return err
				}
			case 'r':
				if err := p.emitJSONStringByte(sink, '\r'); err != nil {
					return err
				}
			case 't':
				if err := p.emitJSONStringByte(sink, '\t'); err != nil {
					return err
				}
			case 'u':
				if rejectUnicodeEscape {
					return errVideoBase64UnicodeEscape
				}
				codeUnit, err := p.readUnicodeCodeUnit(copyRaw)
				if err != nil {
					return err
				}
				if codeUnit >= 0xD800 && codeUnit <= 0xDBFF {
					prefix, err := p.readStringByte(copyRaw)
					if err != nil {
						return p.invalidRead(err)
					}
					marker, err := p.readStringByte(copyRaw)
					if err != nil {
						return p.invalidRead(err)
					}
					if prefix != '\\' || marker != 'u' {
						return errInvalidJSONStringEscape
					}
					low, err := p.readUnicodeCodeUnit(copyRaw)
					if err != nil {
						return err
					}
					if low < 0xDC00 || low > 0xDFFF {
						return errInvalidJSONStringEscape
					}
					decoded := utf16.DecodeRune(rune(codeUnit), rune(low))
					if decoded == utf8.RuneError {
						return errInvalidJSONStringEscape
					}
					if err := emitJSONStringRune(sink, decoded); err != nil {
						return err
					}
				} else if codeUnit >= 0xDC00 && codeUnit <= 0xDFFF {
					return errInvalidJSONStringEscape
				} else {
					if err := emitJSONStringRune(sink, rune(codeUnit)); err != nil {
						return err
					}
				}
			default:
				return errInvalidJSONStringEscape
			}
		default:
			if value < 0x20 {
				return errInvalidVideoJSON
			}
			if value < utf8.RuneSelf {
				if err := p.emitJSONStringByte(sink, value); err != nil {
					return err
				}
				continue
			}
			if err := p.scanRawUTF8(value, copyRaw, sink); err != nil {
				return err
			}
		}
	}
}

func (p *jsonExtractor) scanRawUTF8(
	first byte,
	copyRaw bool,
	sink jsonStringSink,
) error {
	length := utf8SequenceLength(first)
	if length == 0 {
		return errInvalidVideoJSON
	}
	var encoded [utf8.UTFMax]byte
	encoded[0] = first
	for index := 1; index < length; index++ {
		value, err := p.readStringByte(copyRaw)
		if err != nil {
			return p.invalidRead(err)
		}
		encoded[index] = value
	}
	decoded, size := utf8.DecodeRune(encoded[:length])
	if decoded == utf8.RuneError && size == 1 {
		return errInvalidVideoJSON
	}
	return emitJSONStringBytes(sink, encoded[:length])
}

func (p *jsonExtractor) readUnicodeCodeUnit(copyRaw bool) (uint16, error) {
	var digits [4]byte
	for index := range digits {
		value, err := p.readStringByte(copyRaw)
		if err != nil {
			return 0, p.invalidRead(err)
		}
		digit, ok := jsonHexValue(value)
		if !ok {
			return 0, errInvalidJSONStringEscape
		}
		digits[index] = digit
	}
	return uint16(digits[0])<<12 |
		uint16(digits[1])<<8 |
		uint16(digits[2])<<4 |
		uint16(digits[3]), nil
}

func (p *jsonExtractor) readStringByte(copyRaw bool) (byte, error) {
	value, err := p.src.ReadByte()
	if err != nil {
		return 0, err
	}
	if copyRaw {
		if err := p.writeRedactedByte(value); err != nil {
			return 0, err
		}
	}
	return value, nil
}

func (p *jsonExtractor) nextNonSpace(copyToken bool) (byte, error) {
	for {
		value, err := p.src.ReadByte()
		if err != nil {
			return 0, err
		}
		if isJSONWhitespace(value) {
			if err := p.writeRedactedByte(value); err != nil {
				return 0, err
			}
			continue
		}
		if copyToken {
			if err := p.writeRedactedByte(value); err != nil {
				return 0, err
			}
		}
		return value, nil
	}
}

func (p *jsonExtractor) readAndCopy() (byte, error) {
	value, err := p.src.ReadByte()
	if err != nil {
		return 0, err
	}
	if err := p.writeRedactedByte(value); err != nil {
		return 0, err
	}
	return value, nil
}

func (p *jsonExtractor) readAndCopyTo(sink jsonStringSink) (byte, error) {
	value, err := p.readAndCopy()
	if err != nil {
		return 0, err
	}
	if err := p.emitJSONStringByte(sink, value); err != nil {
		return 0, err
	}
	return value, nil
}

func (p *jsonExtractor) consumeTrailingWhitespace() error {
	for {
		value, err := p.src.ReadByte()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.writeRedactedByte(value); err != nil {
			return err
		}
		if !isJSONWhitespace(value) {
			return errInvalidVideoJSON
		}
	}
}

func (p *jsonExtractor) requireValueDelimiter() error {
	value, err := p.peek()
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	if isJSONWhitespace(value) || value == ',' || value == '}' || value == ']' {
		return nil
	}
	return errInvalidVideoJSON
}

func (p *jsonExtractor) peek() (byte, error) {
	peeked, err := p.src.Peek(1)
	if err != nil {
		return 0, err
	}
	return peeked[0], nil
}

func (p *jsonExtractor) writeRedacted(value []byte) error {
	_, err := p.redacted.Write(value)
	return err
}

func (p *jsonExtractor) writeRedactedByte(value byte) error {
	return p.redacted.WriteByte(value)
}

func (p *jsonExtractor) writeEncoded(value []byte) error {
	_, err := p.encoded.Write(value)
	return err
}

func (p *jsonExtractor) emitJSONStringByte(
	sink jsonStringSink,
	value byte,
) error {
	if sink == nil {
		return nil
	}
	p.stringByte[0] = value
	return sink(p.stringByte[:])
}

func (p *jsonExtractor) invalidRead(err error) error {
	if err == io.EOF {
		return errInvalidVideoJSON
	}
	return err
}

func emitJSONStringBytes(sink jsonStringSink, value []byte) error {
	if sink == nil {
		return nil
	}
	return sink(value)
}

func emitJSONStringRune(sink jsonStringSink, value rune) error {
	if sink == nil {
		return nil
	}
	var encoded [utf8.UTFMax]byte
	length := utf8.EncodeRune(encoded[:], value)
	return sink(encoded[:length])
}

func isJSONWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func isJSONDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isErrCodeWhitespace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func toLowerASCII(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

func jsonHexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func utf8SequenceLength(first byte) int {
	switch {
	case first >= 0xC2 && first <= 0xDF:
		return 2
	case first >= 0xE0 && first <= 0xEF:
		return 3
	case first >= 0xF0 && first <= 0xF4:
		return 4
	default:
		return 0
	}
}
