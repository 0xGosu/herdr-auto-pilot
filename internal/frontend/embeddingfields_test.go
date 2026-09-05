package frontend_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/0xGosu/herdr-auto-pilot/internal/config"
	"github.com/0xGosu/herdr-auto-pilot/internal/embedder"
	"github.com/0xGosu/herdr-auto-pilot/internal/frontend"
)

// TestEmbeddingTimeoutFieldsDisplayDefaults covers the config surface an
// operator uses to rescue a slow model: each timeout key must RENDER its own
// default rather than an empty or zero value, since a blank field reads as
// "unset" and invites setting a number the code already uses.
func TestEmbeddingTimeoutFieldsDisplayDefaults(t *testing.T) {
	def := config.Default()
	cases := []struct {
		key  string
		want int
	}{
		{"embedding.embed_timeout_ms", embedder.DefaultEmbedTimeoutMs},
		{"embedding.warm_timeout_ms", embedder.DefaultWarmTimeoutMs},
	}
	for _, c := range cases {
		got := frontend.FieldValue(def, c.key)
		if !strings.Contains(got, strconv.Itoa(c.want)) || !strings.Contains(got, "default") {
			t.Errorf("FieldValue(default, %s) = %q, want it to name the in-force default %d", c.key, got, c.want)
		}
	}

	cfg := config.Default()
	cfg.Embedding.EmbedTimeoutMs = 8000
	cfg.Embedding.WarmTimeoutMs = 120000
	for key, want := range map[string]string{
		"embedding.embed_timeout_ms": "8000",
		"embedding.warm_timeout_ms":  "120000",
	} {
		if got := frontend.FieldValue(cfg, key); got != want {
			t.Errorf("FieldValue(%s) = %q, want %q", key, got, want)
		}
	}
}

// TestEmbeddingTimeoutFieldsRejectNegatives pins SetField validation: 0 is the
// "restore the default" sentinel, negatives are a misconfiguration.
func TestEmbeddingTimeoutFieldsRejectNegatives(t *testing.T) {
	app, _ := testApp(t)
	ctx := context.Background()
	for _, key := range []string{"embedding.embed_timeout_ms", "embedding.warm_timeout_ms"} {
		if _, err := app.SetField(ctx, key, "-1"); err == nil {
			t.Errorf("SetField(%s, -1) was accepted; a negative budget is never valid", key)
		}
		if _, err := app.SetField(ctx, key, "0"); err != nil {
			t.Errorf("SetField(%s, 0) rejected; 0 must restore the default: %v", key, err)
		}
	}
}
