package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hejw/knowledge-core/internal/core"
)

func normalizedGenerationLimits(maxFiles, maxNewPages int) (int, int) {
	if maxFiles <= 0 {
		maxFiles = 12
	}
	if maxNewPages <= 0 {
		maxNewPages = 3
	}
	return maxFiles, maxNewPages
}

func sortedUniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// GenerationContractSHA256 identifies every input that changes the durable
// generation contract for unchanged sources.
func GenerationContractSHA256(projectPath string, maxFiles, maxNewPages int) (string, error) {
	purpose, err := os.ReadFile(filepath.Join(projectPath, "purpose.md"))
	if err != nil {
		return "", err
	}
	schema, err := os.ReadFile(filepath.Join(projectPath, "schema.md"))
	if err != nil {
		return "", err
	}
	return generationContractSHA256(string(purpose), string(schema), maxFiles, maxNewPages), nil
}

func generationContractSHA256(purpose, schema string, maxFiles, maxNewPages int) string {
	maxFiles, maxNewPages = normalizedGenerationLimits(maxFiles, maxNewPages)
	value := fmt.Sprintf("llm-wiki-generation-contract-v%d\nmax_file_blocks=%d\nmax_new_pages=%d\nlanguage=follow-source\npurpose:\n%s\nschema:\n%s",
		core.SourceManifestPipelineVersion, maxFiles, maxNewPages, strings.TrimSpace(purpose), strings.TrimSpace(schema))
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func generationPolicyFor(opts ValidateOptions, entry SourceManifestEntry, hasEntry bool, sourceHash, purpose, schema string) (GenerationPolicy, string) {
	maxFiles, maxNewPages := normalizedGenerationLimits(opts.MaxFilesPerTask, opts.MaxNewPagesPerSource)
	contract := generationContractSHA256(purpose, schema, maxFiles, maxNewPages)
	remaining := maxNewPages
	if hasEntry && entry.SHA256 == sourceHash && entry.GenerationContractSHA256 == contract {
		remaining -= entry.NewPageCount
		if remaining < 0 {
			remaining = 0
		}
	}
	if opts.UpdateOnly {
		remaining = 0
	}
	return GenerationPolicy{
		MaxFileBlocks: maxFiles, MaxNewPagesPerSource: maxNewPages,
		RemainingNewPages: remaining, UpdateOnly: opts.UpdateOnly,
	}, contract
}

func generationPolicyText(policy GenerationPolicy) string {
	mode := fmt.Sprintf("You may create at most %d new non-summary pages in this source integration.", policy.RemainingNewPages)
	if policy.UpdateOnly {
		mode = "This is an impact re-integration. You must not create any new non-summary page; update supplied existing pages only."
	}
	return fmt.Sprintf(`Generation budget (hard validation constraints):
- Return at most %d total ---FILE blocks, including the required source-summary and all existing-page updates.
- Return exactly one source-summary ---FILE block.
- %s
- Updating an existing supplied page does not consume the new-page budget.
- Write titles and prose in the primary language of the current source unless purpose.md explicitly requires another language.`, policy.MaxFileBlocks, mode)
}

func effectiveGenerationPolicy(policy GenerationPolicy, existingUpdatePaths int) GenerationPolicy {
	effectiveMax := 1 + policy.RemainingNewPages + existingUpdatePaths
	if effectiveMax < 1 {
		effectiveMax = 1
	}
	if effectiveMax < policy.MaxFileBlocks {
		policy.MaxFileBlocks = effectiveMax
	}
	return policy
}
