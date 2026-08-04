package httpcore

import "github.com/xavidop/mamori"

// Version derives a mamori Value.Version from a response, preferring the
// strongest validator the backend supplied.
//
// The order is ETag, then Last-Modified, then a hash of the body. ETag comes
// first because it is exact: Last-Modified has one-second resolution, so two
// writes inside the same second are indistinguishable, and a body hash costs a
// full read the validators avoid.
//
// resp may be nil, in which case the body hash is used.
func Version(resp *Response, body []byte) string {
	if resp != nil {
		if resp.ETag != "" {
			return resp.ETag
		}
		if resp.LastModified != "" {
			return resp.LastModified
		}
	}
	return mamori.VersionHash(body)
}
