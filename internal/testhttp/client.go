package testhttp

import (
	"net/http"
	"net/http/httptest"
)

func NewClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{handler}}
}

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		defer r.Body.Close()
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	response := recorder.Result()
	response.Request = r
	return response, nil
}
