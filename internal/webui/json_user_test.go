package webui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shukiv/gniza/internal/answer"
)

// TestACustomerIsNotAnsweredWithData: the account-facing pages print a
// subset of their view on purpose; the rest carries repository addresses
// and root-owned paths. A customer who asks for the page as data gets
// the page.
func TestACustomerIsNotAnsweredWithData(t *testing.T) {
	server, err := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/logs", nil)
	request.Header.Set("Accept", answer.Header)
	request = request.WithContext(context.WithValue(request.Context(), accountKey{}, "studio"))
	response := httptest.NewRecorder()
	server.renderUser(response, request, "user_logs.html", userView{Account: "studio"})
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("a customer asking for data was answered %q: %s", got, response.Body.String())
	}
}
