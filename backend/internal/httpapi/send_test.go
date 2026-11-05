package httpapi

import (
	"net/http"
	"testing"
)

// The send route decodes strictly before any service call: ids, required
// fields, types, unknown and duplicate properties, media type. (Database
// behaviour, idempotent repeats and the 202/200 responses are covered by
// test/integration.)
func TestSendRouteRejectsInvalidRequests(t *testing.T) {
	f := newApprovalFixture(t)
	path := "/api/v1/campaigns/" + testCID + "/send"
	for _, tc := range []struct {
		name, path, body string
		wantPaths        []string
	}{
		{"bad campaign id", "/api/v1/campaigns/nope/send", `{"approval_id":"` + testAID + `","expected_version":3}`, []string{"id"}},
		{"empty body", path, ``, []string{""}},
		{"missing fields", path, `{}`, []string{"approval_id", "expected_version"}},
		{"bad approval id", path, `{"approval_id":"not-a-uuid","expected_version":3}`, []string{"approval_id"}},
		{"approval id not a string", path, `{"approval_id":12,"expected_version":3}`, []string{"approval_id"}},
		{"version zero", path, `{"approval_id":"` + testAID + `","expected_version":0}`, []string{"expected_version"}},
		{"version string", path, `{"approval_id":"` + testAID + `","expected_version":"3"}`, []string{"expected_version"}},
		{"unknown property", path, `{"approval_id":"` + testAID + `","expected_version":3,"force":true}`, []string{"force"}},
		{"duplicate property", path, `{"approval_id":"` + testAID + `","approval_id":"` + testAID + `","expected_version":3}`, []string{"approval_id"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.do("POST", tc.path, tc.body)
			env := assertEnvelope(t, rr, http.StatusUnprocessableEntity, CodeValidationFailed)
			details, _ := env["details"].([]any)
			var got []string
			for _, d := range details {
				got = append(got, d.(map[string]any)["path"].(string))
			}
			if len(got) != len(tc.wantPaths) {
				t.Fatalf("details = %v, want paths %v", details, tc.wantPaths)
			}
			for i := range got {
				if got[i] != tc.wantPaths[i] {
					t.Errorf("details = %v, want paths %v", details, tc.wantPaths)
				}
			}
		})
	}
	// GET is not a send.
	rr := f.do("GET", path, "")
	assertEnvelope(t, rr, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

// The cancel route decodes strictly before any service call.
func TestCancelRouteRejectsInvalidRequests(t *testing.T) {
	f := newApprovalFixture(t)
	path := "/api/v1/campaigns/" + testCID + "/cancel"
	for _, tc := range []struct {
		name, path, body string
		wantPaths        []string
	}{
		{"bad campaign id", "/api/v1/campaigns/nope/cancel", `{"expected_version":3}`, []string{"id"}},
		{"empty body", path, ``, []string{""}},
		{"missing version", path, `{}`, []string{"expected_version"}},
		{"version zero", path, `{"expected_version":0}`, []string{"expected_version"}},
		{"version float", path, `{"expected_version":1.5}`, []string{"expected_version"}},
		{"unknown property", path, `{"expected_version":3,"force":true}`, []string{"force"}},
		{"duplicate property", path, `{"expected_version":3,"expected_version":4}`, []string{"expected_version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.do("POST", tc.path, tc.body)
			env := assertEnvelope(t, rr, http.StatusUnprocessableEntity, CodeValidationFailed)
			details, _ := env["details"].([]any)
			var got []string
			for _, d := range details {
				got = append(got, d.(map[string]any)["path"].(string))
			}
			if len(got) != len(tc.wantPaths) {
				t.Fatalf("details = %v, want paths %v", details, tc.wantPaths)
			}
			for i := range got {
				if got[i] != tc.wantPaths[i] {
					t.Errorf("details = %v, want paths %v", details, tc.wantPaths)
				}
			}
		})
	}
	rr := f.do("GET", path, "")
	assertEnvelope(t, rr, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}
