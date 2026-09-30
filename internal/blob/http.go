package blob

import (
	"errors"
	"fmt"
	"strings"

	awshttp "github.com/aws/smithy-go/transport/http"
)

// isNotFoundStatus reads a 404 off the transport when the SDK did not model
// one.
//
// A HEAD has no body, so there is no error code for the SDK to parse and
// whether it produces a typed *types.NotFound depends on the store. The status
// is the fact either way.
func isNotFoundStatus(err error) bool {
	var resp *awshttp.ResponseError
	if errors.As(err, &resp) {
		return resp.HTTPStatusCode() == 404
	}
	return false
}

// contentDisposition asks the store to serve an artefact as a download named
// after its label.
//
// The filename is quoted and every character that could end the quoting or
// add a header is dropped rather than escaped. A label reaches this from a
// generating tool, and a header is not the place to find out that it contained
// a carriage return.
func contentDisposition(filename string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '\\' || r == ';' || r == ',':
			return -1
		case r < 0x20 || r == 0x7f:
			return -1
		case r > 0x7e:
			// Non-ASCII in an unencoded header value is not interoperable.
			// Dropped rather than RFC 5987-encoded: the label is a
			// convenience, and the artefact id is the name that matters.
			return -1
		default:
			return r
		}
	}, filename)
	safe = strings.TrimSpace(safe)
	if safe == "" {
		return "attachment"
	}
	return fmt.Sprintf("attachment; filename=%q", safe)
}
