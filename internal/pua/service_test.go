package pua

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServiceExportsPlainOutputIsDeterministic(t *testing.T) {
	responses := []string{
		`{"variables":{"ZETA":"last","ALPHA":"first","MIDDLE":"value=with spaces"},"secrets":[{"name":"API_KEY"},{"name":"TOKEN"}]}`,
		`{"variables":{"MIDDLE":"value=with spaces","ZETA":"last","ALPHA":"first"},"secrets":[{"name":"API_KEY"},{"name":"TOKEN"}]}`,
		`{"variables":{"ALPHA":"first","MIDDLE":"value=with spaces","ZETA":"last"},"secrets":[{"name":"API_KEY"},{"name":"TOKEN"}]}`,
	}
	want := "ALPHA=first\n" +
		"MIDDLE=value=with spaces\n" +
		"ZETA=last\n" +
		"API_KEY=<secret:API_KEY>\n" +
		"TOKEN=<secret:TOKEN>\n"

	withTempCwd(t, func(_ string) {
		run(t, "init")
		request := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/services/example/exports") {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(responses[request%len(responses)]))
			request++
		}))
		defer server.Close()

		for i := 0; i < len(responses)*10; i++ {
			if got := run(t, "service", "exports", "example", "--server="+server.URL); got != want {
				t.Fatalf("run %d: service exports = %q, want %q", i, got, want)
			}
		}
	})
}
