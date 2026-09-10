package upstream

import "net/http"

var responseIDFields = []string{"request_id", "requestId", "requestID", "record_id", "recordId", "recordID", "id"}

var responseIDHeaders = []string{
	"X-Request-Id",
	"Request-Id",
	"X-Record-Id",
	"Record-Id",
	"X-Request-Trace-Id",
	"X-Trace-Id",
	"Trace-Id",
}

// ResponseID returns the identifier assigned by WorkBuddy without changing
// its value. The backend has used both OpenAI's id field and request/record ID
// field names across versions.
func ResponseID(obj map[string]any) string {
	for _, key := range responseIDFields {
		if id, ok := obj[key].(string); ok && id != "" {
			return id
		}
	}
	return ""
}

// ResponseIDFromHeader returns the first WorkBuddy request/record identifier
// found in the upstream response headers, preserving its value verbatim.
func ResponseIDFromHeader(header http.Header) string {
	for _, name := range responseIDHeaders {
		if id := header.Get(name); id != "" {
			return id
		}
	}
	return ""
}

// CopyResponseIDHeaders forwards only request/record tracing headers. Other
// upstream headers are deliberately kept private from the downstream client.
func CopyResponseIDHeaders(dst, src http.Header) {
	for _, name := range responseIDHeaders {
		values := src.Values(name)
		if len(values) == 0 {
			continue
		}
		dst.Del(name)
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}

func copyResponseIDFields(dst, src map[string]any) {
	for _, key := range responseIDFields {
		if value, ok := src[key]; ok && value != nil {
			dst[key] = value
		}
	}
}
