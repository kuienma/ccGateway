package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/zhshl/bedrock-anthropic-gateway/internal/anthropic"
)

func (s *Server) prepare(w http.ResponseWriter, r *http.Request) (anthropic.PreparedRequest, bool) {
	raw, err := readBody(w, r, s.maxBodyBytes)
	if err != nil {
		status := http.StatusBadRequest
		message := "读取请求体失败"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			status = http.StatusRequestEntityTooLarge
			message = "请求体超过大小限制"
		}
		s.writeError(w, r, status, anthropic.ErrorTypeInvalidRequest, message)
		return anthropic.PreparedRequest{}, false
	}

	prepared, requestErr := anthropic.PrepareRequest(raw, betaFeatures(r.Header.Values("anthropic-beta")), s.resolver)
	if requestErr != nil {
		status := http.StatusBadRequest
		if requestErr.Kind == anthropic.ErrorTypeNotFound {
			status = http.StatusNotFound
		}
		s.writeError(w, r, status, requestErr.Kind, requestErr.Message)
		return anthropic.PreparedRequest{}, false
	}
	return prepared, true
}

func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func betaFeatures(values []string) []string {
	var result []string
	for _, value := range values {
		for _, feature := range strings.Split(value, ",") {
			if feature = strings.TrimSpace(feature); feature != "" {
				result = append(result, feature)
			}
		}
	}
	return result
}
