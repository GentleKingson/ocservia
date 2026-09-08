package app

import (
	"context"
	"errors"
	"testing"

	"github.com/GentleKingson/ocservia/control-plane/internal/auth"
	"github.com/GentleKingson/ocservia/control-plane/internal/platform/config"
)

func TestBootstrapPasswordPolicyBeforeStartup(t *testing.T) {
	for _, password := range []string{"x", "12345678901234567890", "ocserviapassword"} {
		// No database or logger: policy rejection must precede startup side effects.
		err := Run(context.Background(), config.Config{BootstrapLocalAdmin: true, LocalBootstrapPassword: password}, BuildInfo{}, nil)
		if !errors.Is(err, auth.ErrPasswordPolicy) {
			t.Fatalf("bootstrap reached startup before policy rejection: %v", err)
		}
	}
}

func TestOperationAuthIsNotEnabledBySimulator(t *testing.T) {
	if operationAuthEnabled(config.Config{LocalSimulator: true}) {
		t.Fatal("local simulator enabled operation authentication")
	}
	if !operationAuthEnabled(config.Config{DevAuth: true, LocalSimulator: true}) {
		t.Fatal("development authentication was not enabled")
	}
}
