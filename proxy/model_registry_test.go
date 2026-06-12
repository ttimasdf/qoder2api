package proxy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ttimasdf/qoder2api/database"
)

func newTestModelRegistryDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseOfficialCodexModelIDs(t *testing.T) {
	html := `
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-5.5&quot;]}"></astro-island>
		<code>codex -m gpt-5.4</code>
		<code>codex -m gpt-5.3-codex-spark</code>
		<code>codex -m gpt-5.2</code>
		<code>codex -m gpt-5.2-codex</code>
		<code>codex -m gpt-4.1</code>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	for _, model := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.3-codex-spark", "gpt-5.2"} {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	for _, model := range []string{"gpt-5.2-codex", "gpt-4.1"} {
		if !slices.Contains(skipped, model) {
			t.Fatalf("skipped models missing %q in %v", model, skipped)
		}
	}
}

func TestReasoningEffortModelsAreIncludedInCatalog(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings error: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{
			SiteName:                         "CodexProxy",
			MaxConcurrency:                   2,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			UsageProbeConcurrency:            16,
			RecoveryProbeIntervalMinutes:     30,
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			MaxRetries:                       2,
			MaxRateLimitRetries:              1,
			ModelMapping:                     "{}",
			CodexModelMapping:                "{}",
			PromptFilterMode:                 "monitor",
			PromptFilterThreshold:            50,
			PromptFilterStrictThreshold:      90,
			PromptFilterLogMatches:           true,
			PromptFilterMaxTextLength:        81920,
			PromptFilterCustomPatterns:       "[]",
			PromptFilterDisabledPatterns:     "[]",
			ClientCompatMode:                 "preserve",
			CodexMinCLIVersion:               "0.118.0",
			UsageLogMode:                     "full",
			UsageLogBatchSize:                200,
			UsageLogFlushIntervalSeconds:     5,
			StreamFlushPolicy:                "immediate",
			StreamFlushIntervalMS:            20,
			BillingTierPolicy:                "actual",
			ImageStorageConfig:               "{}",
			SchedulerMode:                    "round_robin",
			AffinityMode:                     "bounded",
			BackgroundConfig:                 "{}",
		}
	}
	settings.ReasoningEffortModels = `[{"model":"gpt-5.5","effort":"xhigh"}]`
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings error: %v", err)
	}

	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		t.Fatalf("ListModelCatalog error: %v", err)
	}
	if !slices.Contains(catalog.Models, "gpt-5.5(xhigh)") {
		t.Fatalf("catalog models missing reasoning alias: %v", catalog.Models)
	}

	var aliasInfo *ModelInfo
	for i := range catalog.Items {
		if catalog.Items[i].ID == "gpt-5.5(xhigh)" {
			aliasInfo = &catalog.Items[i]
			break
		}
	}
	if aliasInfo == nil {
		t.Fatalf("catalog items missing reasoning alias: %#v", catalog.Items)
	}
	if aliasInfo.Source != ModelSourceReasoningEffort {
		t.Fatalf("alias source = %q, want %q", aliasInfo.Source, ModelSourceReasoningEffort)
	}
	if aliasInfo.Category != ModelCategoryCodex {
		t.Fatalf("alias category = %q, want %q", aliasInfo.Category, ModelCategoryCodex)
	}
	if slices.Contains(TextTestModelIDs(ctx, db), "gpt-5.5(xhigh)") {
		t.Fatalf("reasoning alias should not be used for direct connection tests")
	}
}
