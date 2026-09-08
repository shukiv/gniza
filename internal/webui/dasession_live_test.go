//go:build directadmin_live

package webui

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/shukiv/gniza/internal/directadmin"
)

// These run against a real DirectAdmin panel and only against one. They
// are read-only: they ask DirectAdmin about a session that does not
// exist, which is the same request any expired page makes. Nothing is
// installed, nothing is configured and no account is touched.
//
// What they establish is the part no fake can: that the panel's address
// is read correctly out of its own configuration, that its certificate
// verifies through the pool Gniza builds, and that DirectAdmin's answer
// to a session it does not know is read as a refusal rather than as an
// outage.
func liveVerifier(t *testing.T) *daSessionVerifier {
	t.Helper()
	if os.Getenv("GNIZA_DA_LIVE_PANEL") != "yes" {
		t.Skip("requires a real DirectAdmin panel on this host")
	}
	panelURL, err := directadmin.PanelURL(os.Getenv("GNIZA_DA_CONF"))
	if err != nil {
		t.Fatalf("this host does not say where its panel is: %v", err)
	}
	t.Logf("the panel says it answers at %s", panelURL)
	return newDASessionVerifier(panelURL)
}

// A refusal, and not an outage, is the whole point: an outage would mean
// the request never got as far as DirectAdmin, which is what a wrong
// address or an unverifiable certificate looks like.
func TestLiveDirectAdminRefusesASessionItDoesNotKnow(t *testing.T) {
	verifier := liveVerifier(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	who, err := verifier.verify(ctx, "gniza_not_a_real_session", "gniza_not_a_real_key")
	if err == nil {
		t.Fatalf("a session that does not exist was accepted, as %+v", who)
	}
	if errors.Is(err, errDASessionUnavailable) {
		t.Fatalf("the request never reached DirectAdmin: %v", err)
	}
	if !errors.Is(err, errDASessionDenied) {
		t.Fatalf("DirectAdmin's answer was read as neither a refusal nor an outage: %v", err)
	}
	t.Logf("DirectAdmin refused it: %v", err)
}

// The certificate is checked. A panel reached by an address rather than
// by the name its certificate carries must fail, or the check is not
// happening at all.
func TestLiveTheCertificateIsActuallyChecked(t *testing.T) {
	if os.Getenv("GNIZA_DA_LIVE_PANEL") != "yes" {
		t.Skip("requires a real DirectAdmin panel on this host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	verifier := newDASessionVerifier("https://127.0.0.1:2222")
	_, err := verifier.verify(ctx, "gniza_not_a_real_session", "gniza_not_a_real_key")
	if !errors.Is(err, errDASessionUnavailable) {
		t.Fatalf("an address the certificate does not name was reached anyway: %v", err)
	}
	t.Logf("refused an unnamed address: %v", err)
}
