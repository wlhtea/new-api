package types

// ErrorOrigin records the trust boundary that produced a relay error. Public
// projection, retry, billing, and health policy must use this value instead of
// inferring provenance from free-form error text.
type ErrorOrigin string

const (
	ErrorOriginUnknown            ErrorOrigin = "unknown"
	ErrorOriginLocalValidation    ErrorOrigin = "local.validation"
	ErrorOriginLocalCancel        ErrorOrigin = "local.cancel"
	ErrorOriginLocalDeadline      ErrorOrigin = "local.deadline"
	ErrorOriginLocalWriter        ErrorOrigin = "local.writer"
	ErrorOriginLocalPanic         ErrorOrigin = "local.panic"
	ErrorOriginGatewayConfig      ErrorOrigin = "gateway.config"
	ErrorOriginGatewayDependency  ErrorOrigin = "gateway.dependency"
	ErrorOriginGatewayInvariant   ErrorOrigin = "gateway.invariant"
	ErrorOriginUpstreamTransport  ErrorOrigin = "upstream.transport"
	ErrorOriginUpstreamHTTP       ErrorOrigin = "upstream.http"
	ErrorOriginUpstreamEnvelope   ErrorOrigin = "upstream.envelope"
	ErrorOriginUpstreamMalformed  ErrorOrigin = "upstream.malformed"
	ErrorOriginUpstreamIncomplete ErrorOrigin = "upstream.incomplete"
)

// ErrorProvenance is internal policy evidence. Subtype is a stable local
// identifier, never upstream text. RawStatusCode is the status observed at the
// upstream boundary before mapping; zero means no raw HTTP status exists.
type ErrorProvenance struct {
	Origin        ErrorOrigin
	Subtype       string
	RawStatusCode int
}

func (p ErrorProvenance) IsZero() bool {
	return p.Origin == "" && p.Subtype == "" && p.RawStatusCode == 0
}

func (p ErrorProvenance) IsUpstream() bool {
	switch p.Origin {
	case ErrorOriginUpstreamTransport,
		ErrorOriginUpstreamHTTP,
		ErrorOriginUpstreamEnvelope,
		ErrorOriginUpstreamMalformed,
		ErrorOriginUpstreamIncomplete:
		return true
	default:
		return false
	}
}

func (p ErrorProvenance) IsLocal() bool {
	switch p.Origin {
	case ErrorOriginLocalValidation,
		ErrorOriginLocalCancel,
		ErrorOriginLocalDeadline,
		ErrorOriginLocalWriter,
		ErrorOriginLocalPanic:
		return true
	default:
		return false
	}
}

func (p ErrorProvenance) IsGateway() bool {
	return p.Origin == ErrorOriginGatewayConfig ||
		p.Origin == ErrorOriginGatewayDependency ||
		p.Origin == ErrorOriginGatewayInvariant
}

// Provenance returns a copy so callers cannot mutate the error's policy
// evidence after it has crossed a trust boundary.
func (e *NewAPIError) Provenance() ErrorProvenance {
	if e == nil {
		return ErrorProvenance{}
	}
	return e.provenance
}

// SetProvenance records the first provenance assignment. Repeating the exact
// assignment is idempotent; a conflicting assignment fails closed and leaves
// the original evidence unchanged.
func (e *NewAPIError) SetProvenance(provenance ErrorProvenance) bool {
	if e == nil || provenance.IsZero() {
		return false
	}
	if e.provenance.IsZero() {
		e.provenance = provenance
		return true
	}
	return e.provenance == provenance
}

func ErrOptionWithProvenance(provenance ErrorProvenance) NewAPIErrorOptions {
	return func(e *NewAPIError) {
		e.SetProvenance(provenance)
	}
}
