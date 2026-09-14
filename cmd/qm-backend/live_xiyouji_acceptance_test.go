package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qmagent "github.com/loon-hejw/knowlega/internal/qm/agent"
	"github.com/loon-hejw/knowlega/internal/qm/config"
)

const xiyoujiNineConditions = `根据提示猜西游记人物： 1.有结义的情节

2.见过孙悟空

3.跟孙悟空不算敌对关系

4.出场不止一次

5.曾逼迫唐僧做了某事

6.最后唐僧就范,而且没受到什么损失

7.见过阎罗王

8.见过观音

9.不曾到过花果山`

// This opt-in acceptance reads an existing project. It never truncates the
// database, ingests sources, or grants model writeback access. Setting the config
// path explicitly opts into sending project evidence to its configured provider.
// Reports contain evidence and are private artifacts, not commit candidates.
func TestLiveXiyoujiPiAcceptance(t *testing.T) {
	configPath := os.Getenv("QM_XIYOUJI_ACCEPTANCE_CONFIG")
	if configPath == "" {
		t.Skip("set QM_XIYOUJI_ACCEPTANCE_CONFIG after authorizing external model access")
	}
	scope := os.Getenv("QM_XIYOUJI_ACCEPTANCE_SCOPE")
	reportDir := os.Getenv("QM_XIYOUJI_ACCEPTANCE_REPORT_DIR")
	if scope == "" || reportDir == "" {
		t.Fatal("QM_XIYOUJI_ACCEPTANCE_SCOPE and QM_XIYOUJI_ACCEPTANCE_REPORT_DIR are required")
	}
	configPath = liveLLMConfigPath(t, configPath)
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cfg.Knowledge.RootDir) {
		cfg.Knowledge.RootDir = filepath.Join(filepath.Dir(configPath), cfg.Knowledge.RootDir)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	store, closeStore, err := openKnowledgeStore(ctx, cfg.Database.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	knowledge, err := buildKnowledgeAgent(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(reportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 3; run++ {
		t.Run(fmt.Sprintf("fresh-%d", run), func(t *testing.T) {
			session := &livePiSession{sessionID: fmt.Sprintf("xiyouji-acceptance-%d-%d", time.Now().UnixNano(), run)}
			payload, _ := json.Marshal(map[string]string{"text": xiyoujiNineConditions})
			if _, err := session.Emit(ctx, qmagent.NewEntry{Type: "user", Payload: payload, ScopeLabel: scope}); err != nil {
				t.Fatal(err)
			}
			resolver, err := qmagent.NewCoreToolContextResolver(qmagent.CoreToolContextOptions{Sessions: session, Knowledge: knowledge, WorkspaceRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			start := 0
			tools, err := resolver.ResolveToolContext(ctx, qmagent.TurnTaskPayload{SessionID: session.sessionID, ScopeLabel: scope, OrgScopeID: "org:" + cfg.QM.OrgID, UserEntrySequence: &start, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			input := qmagent.TurnInput{SessionID: session.sessionID, RunID: session.sessionID, Input: xiyoujiNineConditions, SystemPrompt: "You are QM's outer Pi reasoning agent. Answer project questions using the available project evidence. Explain uncertainty honestly.", ScopeLabel: scope, OrgScopeID: "org:" + cfg.QM.OrgID, Model: cfg.QM.Models.DefaultModel(), Harness: "pi", UserEntrySequence: &start, Tools: tools, Emit: session.Emit}
			if _, err := qmagent.NewProjectKnowledgePreparer().PrepareTurnInput(ctx, &input); err != nil {
				t.Fatal(err)
			}
			adapter, err := qmagent.NewPiAdapter(cfg.QM.Models, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer adapter.Close(context.Background())
			result, runErr := adapter.RunTurn(ctx, input)
			errorText := ""
			if runErr != nil {
				errorText = runErr.Error()
			}
			report, err := json.MarshalIndent(map[string]any{"model": cfg.QM.Models.DefaultModel(), "result": result, "error": errorText, "entries": session.entries}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(reportDir, session.sessionID+".json"), report, 0o600); err != nil {
				t.Fatal(err)
			}
			if runErr != nil || strings.TrimSpace(result.Reply) == "" {
				t.Fatalf("run failed: %v; inspect saved trace", runErr)
			}
			t.Logf("model=%s model_calls=%d tool_calls=%d; saved trace requires evidence review", cfg.QM.Models.DefaultModel(), result.ModelCalls, result.ToolCalls)
		})
	}
}
