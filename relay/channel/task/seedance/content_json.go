package seedance

import (
	"bufio"
	"errors"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	errInvalidVideoJSON         = errors.New("invalid video JSON")
	errInvalidJSONStringEscape  = errors.New("invalid JSON string escape")
	errVideoBase64Missing       = errors.New("video_base64 is missing")
	errDuplicateVideoBase64     = errors.New("duplicate video_base64")
	errVideoBase64NotString     = errors.New("video_base64 must be a string")
	errVideoBase64UnicodeEscape = errors.New("video_base64 must not use Unicode escapes")
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

type jsonFrame struct {
	object bool
	state  jsonScanState
}

type jsonStringSink func([]byte) error

type jsonExtractor struct {
	src      *bufio.Reader
	redacted *bufio.Writer
	encoded  *bufio.Writer

	videoBase64Seen    bool
	videoBase64Present bool
	targetValuePending bool
}

type rootVideoKeyMatcher struct {
	index int
	match bool
}

func newRootVideoKeyMatcher() *rootVideoKeyMatcher {
	return &rootVideoKeyMatcher{match: true}
}

func (m *rootVideoKeyMatcher) Write(decoded []byte) error {
	const target = "video_base64"
	for _, value := range decoded {
		if m.index >= len(target) || value != target[m.index] {
			m.match = false
		}
		m.index++
	}
	return nil
}

func (m *rootVideoKeyMatcher) Matches() bool {
	return m.match && m.index == len("video_base64")
}

// extractVideoBase64JSON consumes one JSON root object, writes a safe redacted
// copy, and streams the decoded root-level video_base64 JSON string to encoded.
func extractVideoBase64JSON(
	src io.Reader,
	redacted io.Writer,
	encoded io.Writer,
) error {
	if src == nil || redacted == nil || encoded == nil {
		return errInvalidVideoJSON
	}

	extractor := &jsonExtractor{
		src:      bufio.NewReaderSize(src, 32*1024),
		redacted: bufio.NewWriterSize(redacted, 32*1024),
		encoded:  bufio.NewWriterSize(encoded, 32*1024),
	}
	if err := extractor.scan(); err != nil {
		return err
	}
	if err := extractor.encoded.Flush(); err != nil {
		return err
	}
	return extractor.redacted.Flush()
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
	afterComma := []bool{false}
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
				if afterComma[index] {
					return errInvalidVideoJSON
				}
				p.completeContainer(&frames, &afterComma)
				continue
			}
			if keyStart != '"' {
				return errInvalidVideoJSON
			}

			var matcher *rootVideoKeyMatcher
			var keySink jsonStringSink
			if len(frames) == 1 {
				matcher = newRootVideoKeyMatcher()
				keySink = matcher.Write
			}
			if err := p.scanJSONString(true, keySink, false); err != nil {
				return err
			}

			if matcher != nil {
				p.targetValuePending = matcher.Matches()
				if p.targetValuePending {
					if p.videoBase64Seen {
						return errDuplicateVideoBase64
					}
					p.videoBase64Seen = true
				}
			}
			frames[index].state = scanObjectColon
			afterComma[index] = false

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
			target := len(frames) == 1 && p.targetValuePending
			valueStart, err := p.nextNonSpace(!target)
			if err != nil {
				return p.invalidRead(err)
			}
			if err := p.scanValueFromStart(
				&frames,
				&afterComma,
				valueStart,
				target,
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
				afterComma[index] = true
			case '}':
				p.completeContainer(&frames, &afterComma)
			default:
				return errInvalidVideoJSON
			}

		case scanArrayValue:
			valueStart, err := p.nextNonSpace(true)
			if err != nil {
				return p.invalidRead(err)
			}
			if valueStart == ']' {
				if afterComma[index] {
					return errInvalidVideoJSON
				}
				p.completeContainer(&frames, &afterComma)
				continue
			}
			if err := p.scanValueFromStart(
				&frames,
				&afterComma,
				valueStart,
				false,
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
				afterComma[index] = true
			case ']':
				p.completeContainer(&frames, &afterComma)
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
	afterComma *[]bool,
	start byte,
	target bool,
) error {
	if target {
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
		p.targetValuePending = false
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
		*frames = append(*frames, jsonFrame{object: true, state: scanObjectKey})
		*afterComma = append(*afterComma, false)
		return nil
	case '[':
		*frames = append(*frames, jsonFrame{object: false, state: scanArrayValue})
		*afterComma = append(*afterComma, false)
		return nil
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
		if err := p.scanNumber(start); err != nil {
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

func (p *jsonExtractor) completeContainer(
	frames *[]jsonFrame,
	afterComma *[]bool,
) {
	*frames = (*frames)[:len(*frames)-1]
	*afterComma = (*afterComma)[:len(*afterComma)-1]
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

func (p *jsonExtractor) scanNumber(first byte) error {
	current := first
	if current == '-' {
		value, err := p.readAndCopy()
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
			if _, err := p.readAndCopy(); err != nil {
				return p.invalidRead(err)
			}
		}
	default:
		return errInvalidVideoJSON
	}

	if next, err := p.peek(); err == nil && next == '.' {
		if _, err := p.readAndCopy(); err != nil {
			return p.invalidRead(err)
		}
		fraction, err := p.readAndCopy()
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
			if _, err := p.readAndCopy(); err != nil {
				return p.invalidRead(err)
			}
		}
	} else if err != nil && err != io.EOF {
		return err
	}

	if next, err := p.peek(); err == nil && (next == 'e' || next == 'E') {
		if _, err := p.readAndCopy(); err != nil {
			return p.invalidRead(err)
		}
		if sign, err := p.peek(); err == nil && (sign == '+' || sign == '-') {
			if _, err := p.readAndCopy(); err != nil {
				return p.invalidRead(err)
			}
		} else if err != nil && err != io.EOF {
			return err
		}
		exponent, err := p.readAndCopy()
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
			if _, err := p.readAndCopy(); err != nil {
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
				if err := emitJSONStringBytes(sink, []byte{escaped}); err != nil {
					return err
				}
			case 'b':
				if err := emitJSONStringBytes(sink, []byte{'\b'}); err != nil {
					return err
				}
			case 'f':
				if err := emitJSONStringBytes(sink, []byte{'\f'}); err != nil {
					return err
				}
			case 'n':
				if err := emitJSONStringBytes(sink, []byte{'\n'}); err != nil {
					return err
				}
			case 'r':
				if err := emitJSONStringBytes(sink, []byte{'\r'}); err != nil {
					return err
				}
			case 't':
				if err := emitJSONStringBytes(sink, []byte{'\t'}); err != nil {
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
				if err := emitJSONStringBytes(sink, []byte{value}); err != nil {
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
		if err := p.writeRedacted([]byte{value}); err != nil {
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
			if err := p.writeRedacted([]byte{value}); err != nil {
				return 0, err
			}
			continue
		}
		if copyToken {
			if err := p.writeRedacted([]byte{value}); err != nil {
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
	if err := p.writeRedacted([]byte{value}); err != nil {
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
		if err := p.writeRedacted([]byte{value}); err != nil {
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

func (p *jsonExtractor) writeEncoded(value []byte) error {
	_, err := p.encoded.Write(value)
	return err
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
