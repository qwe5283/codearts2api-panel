package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestPanelEntrypointsAndAssets(t *testing.T) {
	h := NewHandler(Config{APIKey: "test-key"})
	for _, path := range []string{"/", "/admin", "/panel", "/panel/"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "CodeArts2API") {
				t.Fatalf("panel unavailable: %d", w.Code)
			}
			for _, asset := range []string{"/panel/assets/app.css", "/panel/assets/app.js"} {
				if !strings.Contains(w.Body.String(), asset) {
					t.Fatalf("missing asset %s", asset)
				}
			}
		})
	}
	for _, test := range []struct{ path, mime string }{
		{"/panel/assets/app.js", "text/javascript"},
		{"/panel/assets/app.css", "text/css"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, test.path, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), test.mime) || w.Body.Len() == 0 {
			t.Fatalf("invalid asset response %s: %d, %s", test.path, w.Code, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatal("missing static response security headers")
		}
	}
	for _, path := range []string{"/panel/assets/nope.js", "/panel/assets/../secret"} {
		w := httptest.NewRecorder()
		h.servePanelAsset(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("unknown asset %s returned %d", path, w.Code)
		}
	}
}

func TestPanelMarkupMatchesScriptBindings(t *testing.T) {
	cards := regexp.MustCompile(`<div class="stat">`).FindAllString(regexp.MustCompile(`(?s)<div class="stats-grid accounts-stats">(.*?)</section>`).FindStringSubmatch(panelHTMLRaw)[1], -1)
	if len(cards) != 6 {
		t.Fatalf("account stat cards = %d, want 6", len(cards))
	}
	if !strings.Contains(panelCSS, ".accounts-stats{grid-template-columns:repeat(6,minmax(0,1fr))}") ||
		!strings.Contains(panelCSS, ".stats-grid.usage,.accounts-stats{grid-template-columns:repeat(2,minmax(0,1fr))}") {
		t.Fatal("account stats must use six columns and fall back to two columns")
	}
	ids := make(map[string]bool)
	for _, m := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(panelHTMLRaw, -1) {
		if ids[m[1]] {
			t.Fatalf("duplicate element ID: %s", m[1])
		}
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\$\('([^']+)'\)`).FindAllStringSubmatch(panelJS, -1) {
		if !ids[m[1]] {
			t.Errorf("script binding has no element: %s", m[1])
		}
	}
	for _, name := range []string{"accounts", "balance", "models", "usage", "config", "logs"} {
		if !ids["view-"+name] {
			t.Errorf("missing view: %s", name)
		}
	}
	for _, value := range []string{"first", "credits", "round_robin"} {
		if !strings.Contains(panelHTMLRaw, `<option value="`+value+`">`) {
			t.Errorf("missing account strategy option %q", value)
		}
	}
	if !strings.Contains(panelJS, `['cfgAccountStrategy', 'account_strategy', 'text']`) {
		t.Error("account strategy select is not bound to configuration")
	}
	for _, binding := range []string{"['cfgSessionTTL', 'session_ttl', 'text']", "['cfgReserveCredits', 'reserve_credits', 'float']"} {
		if !strings.Contains(panelJS, binding) {
			t.Errorf("missing scheduling configuration binding %q", binding)
		}
	}
	if strings.Contains(panelHTMLRaw, "<script>") || regexp.MustCompile(`\bon(click|submit|load)=`).MatchString(panelHTMLRaw) {
		t.Fatal("inline script bypasses the panel CSP")
	}
}
