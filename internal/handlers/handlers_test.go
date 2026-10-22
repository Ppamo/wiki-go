package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfigReadMiddlewareExcludesConfigWrites(t *testing.T) {
	readerStarted := make(chan struct{})
	releaseReader := make(chan struct{})
	readerDone := make(chan struct{})

	handler := ConfigReadMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(readerStarted)
		<-releaseReader
	}))
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/check-auth", nil))
		close(readerDone)
	}()
	<-readerStarted

	writerAcquired := make(chan struct{})
	go func() {
		configWriteMu.Lock()
		close(writerAcquired)
		configWriteMu.Unlock()
	}()

	select {
	case <-writerAcquired:
		t.Fatal("configuration writer acquired the lock while a reader was active")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseReader)
	select {
	case <-writerAcquired:
	case <-time.After(time.Second):
		t.Fatal("configuration writer remained blocked after the reader completed")
	}
	<-readerDone
}

func TestConfigReadMiddlewareAllowsMutatingHandlerToTakeWriteLock(t *testing.T) {
	paths := []string{
		"/api/password",
		"/api/tokens",
		"/api/users",
		"/api/settings/wiki",
		"/api/settings/security",
		"/api/access-rules",
		"/api/access-rules/0",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			handler := ConfigReadMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				configWriteMu.Lock()
				defer configWriteMu.Unlock()
			}))

			done := make(chan struct{})
			go func() {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("mutating handler deadlocked while taking the configuration write lock")
			}
		})
	}
}
