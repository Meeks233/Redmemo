package handler

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/redmemo/redmemo/internal/totp"
)

// TestEnrollQRDataURISurvivesTemplate guards against the html/template URL
// filter blanking the TOTP QR. html/template only trusts http/https/mailto in
// a src= context; a plain-string data: URI is rewritten to "#ZgotmplZ", which
// shows the enrollment page with a broken image and only the manual secret
// text. QRDataURI must therefore be template.URL so the data URI renders.
func TestEnrollQRDataURISurvivesTemplate(t *testing.T) {
	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	dataURI, err := qrDataURI(secret)
	if err != nil {
		t.Fatalf("qrDataURI: %v", err)
	}
	if !strings.HasPrefix(dataURI, "data:image/png;base64,") {
		t.Fatalf("unexpected data URI prefix: %.32q", dataURI)
	}

	v := authPageView{Stage: "enroll", Secret: secret, QRDataURI: template.URL(dataURI)}
	var buf bytes.Buffer
	if err := authPageTpl.Execute(&buf, v); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "ZgotmplZ") {
		t.Fatal("QR src was filtered to #ZgotmplZ — data URI must be template.URL")
	}
	if !strings.Contains(out, `src="data:image/png;base64,`) {
		t.Fatalf("rendered enroll page missing inline QR data URI")
	}
}
