package api

import (
	"testing"
	"time"

	"github.com/tai-core/tai-talea/internal/config"
)

// A queued creation is only bounded by controller.provision_timeout, because
// the CCI platform never cancels a queued request (queueTimeoutCancelEnable is
// false). The value therefore has to be readable from configuration, and an
// unset one must keep the historical 45 minutes rather than collapsing to zero
// (a zero budget would cancel every creation instantly).
func TestProvisionTimeoutComesFromConfiguration(t *testing.T) {
	zero := &Server{opts: Options{Config: config.Config{}}}
	if got := zero.provisionTimeout(); got != 45*time.Minute {
		t.Fatalf("unset provision_timeout must fall back to 45m, got %s", got)
	}

	cfg := config.Default()
	cfg.Controller.ProvisionTimeout = config.Duration(6 * time.Hour)
	configured := &Server{opts: Options{Config: cfg}}
	if got := configured.provisionTimeout(); got != 6*time.Hour {
		t.Fatalf("provision_timeout must come from configuration, got %s", got)
	}
}
