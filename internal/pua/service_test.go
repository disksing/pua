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

func TestServiceStartSurfacesDisabledServiceConflict(t *testing.T) {
	withTempCwd(t, func(_ string) {
		run(t, "init")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/services/worker/start") {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"service_disabled","error":"start service \"worker\": service is disabled; enable it first"}`))
		}))
		defer server.Close()

		output, err := runErr(t, "service", "start", "worker", "--server="+server.URL)
		if output != "" {
			t.Fatalf("service start output = %q, want empty", output)
		}
		want := `PUA Server service_disabled: start service "worker": service is disabled; enable it first`
		if err == nil || err.Error() != want {
			t.Fatalf("service start error = %v, want %q", err, want)
		}
	})
}
