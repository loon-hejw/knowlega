package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	manifestfile "github.com/loon-hejw/knowlega/internal/agent/knowlega/manifest"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/projectlock"
	"github.com/loon-hejw/knowlega/internal/agent/knowlega/wiki"
	"gopkg.in/yaml.v3"
)

type WikiConvergenceResult struct {
	RewrittenPages          int
	MovedPages              int
	MergedPages             int
	RemovedBadSources       int
	RemovedAmbiguousAliases int
	NormalizedReviews       int
	DowngradedLinks         int
	LinkedSummaries         int
	LinkedContentPages      int
	UnownedPages            int
	QuarantinedPages        int
}

type convergencePage struct {
	path  string
	fm    map[string]any
	body  string
	title string
}

// ConvergeWikiArtifacts repairs deterministic invariants after a batch has
// settled. It never asks an LLM to decide evidence: provenance is restricted
// to real raw files, legacy directories are migrated to canonical paths, and
// colliding legacy pages are losslessly consolidated with archived originals.
func ConvergeWikiArtifacts(projectPath string) (WikiConvergenceResult, error) {
	projectLock, err := projectlock.Acquire(projectPath)
	if err != nil {
		return WikiConvergenceResult{}, err
	}
	defer projectLock.Release()
	wikiPersistMu.Lock()
	defer wikiPersistMu.Unlock()

	manifest, err := loadSourceManifest(projectPath)
	if err != nil {
		return WikiConvergenceResult{}, err
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return WikiConvergenceResult{}, err
	}
	pathMap := map[string]string{}
	groups := map[string][]convergencePage{}
	var result WikiConvergenceResult
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/log.md" || page.Path == "wiki/reviews.md" {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(page.Path)))
		if readErr != nil {
			return result, readErr
		}
		fm, body, parseErr := splitGeneratedFrontmatter(string(content))
		if parseErr != nil {
			continue
		}
		dest := canonicalWikiPathForType(page.Path, page.Type)
		if isProtectedWikiPath(dest) {
			pathMap[page.Path] = ""
			if err := wiki.RemoveVersionedPage(projectPath, page.Path, "remove duplicate generated aggregate during wiki convergence"); err != nil {
				return result, err
			}
			result.MovedPages++
			continue
		}
		pathMap[page.Path] = dest
		groups[dest] = append(groups[dest], convergencePage{path: page.Path, fm: fm, body: body, title: page.Title})
	}
	groups, pathMap = coalesceSemanticDuplicateGroups(groups, pathMap)
	titleTokenOwners := convergenceTitleTokenOwners(groups)

	owners := map[string]map[string]bool{}
	for _, entry := range manifest.Sources {
		if !validExistingRawSource(projectPath, entry.RawPath) {
			continue
		}
		for _, oldPath := range entry.Files {
			dest, ok := pathMap[filepath.ToSlash(oldPath)]
			if !ok {
				dest = filepath.ToSlash(oldPath)
			}
			if dest == "" {
				continue
			}
			if owners[dest] == nil {
				owners[dest] = map[string]bool{}
			}
			owners[dest][filepath.ToSlash(entry.RawPath)] = true
		}
	}
	ambiguousAliases, ambiguousAliasPaths := convergenceAmbiguousAliases(groups, owners)

	for dest, candidates := range groups {
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].path == dest {
				return true
			}
			if candidates[j].path == dest {
				return false
			}
			return len(candidates[i].body) > len(candidates[j].body)
		})
		base := candidates[0]
		aliases := map[string]bool{}
		ambiguousPageAliases := map[string]bool{}
		sources := map[string]bool{}
		for source := range owners[dest] {
			sources[source] = true
		}
		for _, candidate := range candidates {
			for _, alias := range frontmatterStrings(candidate.fm["ambiguous_aliases"]) {
				alias = strings.TrimSpace(alias)
				if alias == "" || strings.EqualFold(alias, base.title) {
					continue
				}
				if ambiguousAliases[normalizeIdentityName(alias)] {
					ambiguousPageAliases[alias] = true
				} else {
					aliases[alias] = true
				}
			}
			for _, alias := range frontmatterStrings(candidate.fm["aliases"]) {
				alias = strings.TrimSpace(alias)
				if ambiguousAliases[normalizeIdentityName(alias)] {
					result.RemovedAmbiguousAliases++
					if alias != "" && !strings.EqualFold(alias, base.title) {
						ambiguousPageAliases[alias] = true
					}
					continue
				}
				if alias != "" && !strings.EqualFold(alias, base.title) && !dropMisassignedAlias(alias, base.title, dest, titleTokenOwners) {
					aliases[alias] = true
				}
			}
			if candidate.title != "" && !strings.EqualFold(candidate.title, base.title) {
				if ambiguousAliases[normalizeIdentityName(candidate.title)] {
					result.RemovedAmbiguousAliases++
					ambiguousPageAliases[candidate.title] = true
				} else {
					aliases[candidate.title] = true
				}
			}
			for _, source := range frontmatterStrings(candidate.fm["sources"]) {
				source = filepath.ToSlash(strings.TrimSpace(source))
				if source != "" && !sources[source] {
					result.RemovedBadSources++
				}
			}
		}
		body := strings.TrimSpace(base.body)
		for _, candidate := range candidates[1:] {
			other := strings.TrimSpace(candidate.body)
			if other == "" || strings.Contains(body, other) || strings.Contains(other, body) {
				continue
			}
			body += "\n\n## Preserved evidence from " + candidate.title + "\n\n" + stripLeadingH1(other)
			result.MergedPages++
		}
		base.fm["type"] = canonicalTypeForPath(dest, strings.TrimSpace(fmt.Sprint(base.fm["type"])))
		base.fm["title"] = base.title
		base.fm["aliases"] = sortedSet(aliases)
		if len(ambiguousPageAliases) > 0 {
			base.fm["ambiguous_aliases"] = sortedSet(ambiguousPageAliases)
		} else {
			delete(base.fm, "ambiguous_aliases")
		}
		base.fm["sources"] = sortedSet(sources)
		fmYAML, marshalErr := yaml.Marshal(base.fm)
		if marshalErr != nil {
			return result, marshalErr
		}
		content := "---\n" + strings.TrimSpace(string(fmYAML)) + "\n---\n\n" + body + "\n"
		before, _ := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(dest)))
		if string(before) != content {
			if err := wiki.WriteVersionedPage(projectPath, dest, []byte(content), "normalize wiki paths and provenance"); err != nil {
				return result, err
			}
			result.RewrittenPages++
		}
		for _, candidate := range candidates {
			if candidate.path == dest {
				continue
			}
			if err := wiki.RemoveVersionedPage(projectPath, candidate.path, "migrate page into canonical wiki path "+dest); err != nil {
				return result, err
			}
			result.MovedPages++
		}
	}

	for key, entry := range manifest.Sources {
		seen := map[string]bool{}
		files := make([]string, 0, len(entry.Files))
		for _, oldPath := range entry.Files {
			oldPath = filepath.ToSlash(oldPath)
			dest, ok := pathMap[oldPath]
			if !ok {
				dest = oldPath
			}
			if dest == "" || seen[dest] {
				continue
			}
			if _, err := os.Stat(filepath.Join(projectPath, filepath.FromSlash(dest))); err != nil {
				continue
			}
			seen[dest] = true
			files = append(files, dest)
		}
		sort.Strings(files)
		entry.Files = files
		createdSeen := map[string]bool{}
		createdPages := make([]string, 0, len(entry.CreatedPages))
		for _, oldPath := range entry.CreatedPages {
			oldPath = filepath.ToSlash(oldPath)
			dest, ok := pathMap[oldPath]
			if !ok || dest == "" {
				dest = oldPath
			}
			if dest == "" || createdSeen[dest] {
				continue
			}
			createdSeen[dest] = true
			createdPages = append(createdPages, dest)
		}
		sort.Strings(createdPages)
		entry.CreatedPages = createdPages
		manifest.Sources[key] = entry
	}
	if err := saveSourceManifest(projectPath, manifest); err != nil {
		return result, err
	}
	if err := rewriteCanonicalWikiLinks(projectPath, pathMap); err != nil {
		return result, err
	}
	if len(ambiguousAliases) > 0 {
		aliases := make([]string, 0, len(ambiguousAliases))
		var affected []string
		affectedSet := map[string]bool{}
		for alias := range ambiguousAliases {
			aliases = append(aliases, alias)
			for path := range ambiguousAliasPaths[alias] {
				if !affectedSet[path] {
					affectedSet[path] = true
					affected = append(affected, path)
				}
			}
		}
		sort.Strings(aliases)
		sort.Strings(affected)
		if err := appendReview(projectPath, "Wiki convergence", "system:repair-wiki", ReviewBlock{
			Type:  "review-needed",
			Title: "Ambiguous aliases removed from navigation metadata",
			Body:  "These names identified more than one active page and were removed as aliases so link resolution cannot choose an arbitrary target. Review the affected pages for a semantic merge or a more specific alias:\n- " + strings.Join(aliases, "\n- "),
		}, affected); err != nil {
			return result, err
		}
	}
	result.NormalizedReviews, err = normalizeReviewTypes(projectPath)
	if err != nil {
		return result, err
	}
	result.LinkedSummaries, err = ensureSourceSummaryOutlinks(projectPath, manifest)
	if err != nil {
		return result, err
	}
	result.LinkedContentPages, err = ensureContentPageOutlinks(projectPath, manifest)
	if err != nil {
		return result, err
	}
	result.QuarantinedPages, err = quarantineUnownedWikiPages(projectPath, manifest)
	if err != nil {
		return result, err
	}
	result.DowngradedLinks, err = downgradeCurrentBrokenLinks(projectPath)
	if err != nil {
		return result, err
	}
	if err := removeEmptyLegacyWikiDirs(projectPath); err != nil {
		return result, err
	}
	result.UnownedPages, err = countUnownedWikiPages(projectPath, manifest)
	return result, err
}

func removeEmptyLegacyWikiDirs(projectPath string) error {
	for _, rel := range []string{"wiki/entity", "wiki/concept", "wiki/location", "wiki/synthesis"} {
		abs := filepath.Join(projectPath, filepath.FromSlash(rel))
		entries, err := os.ReadDir(abs)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func downgradeCurrentBrokenLinks(projectPath string) (int, error) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return 0, err
	}
	targetPages := map[string]map[string]bool{}
	count := 0
	for _, page := range pages {
		abs := filepath.Join(projectPath, filepath.FromSlash(page.Path))
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			return count, readErr
		}
		blocks := ParsedBlocks{Files: []FileBlock{{Path: page.Path, Content: string(data)}}}
		missing := downgradeMissingWikilinks(projectPath, &blocks)
		if len(missing) == 0 {
			continue
		}
		for _, target := range missing {
			if targetPages[target] == nil {
				targetPages[target] = map[string]bool{}
			}
			targetPages[target][page.Path] = true
			count++
		}
		if err := wiki.WriteVersionedPage(projectPath, page.Path, []byte(blocks.Files[0].Content), "downgrade unresolved wikilinks during wiki convergence"); err != nil {
			return count, err
		}
	}
	if len(targetPages) > 0 {
		targets := make([]string, 0, len(targetPages))
		var affected []string
		affectedSet := map[string]bool{}
		for target, paths := range targetPages {
			targets = append(targets, target)
			for path := range paths {
				if !affectedSet[path] {
					affectedSet[path] = true
					affected = append(affected, path)
				}
			}
		}
		sort.Strings(targets)
		sort.Strings(affected)
		if err := appendReview(projectPath, "Wiki convergence", "system:repair-wiki", ReviewBlock{
			Type:  "missing-page",
			Title: "Unresolved links downgraded during structural convergence",
			Body:  "These unresolved targets were rendered as plain text. Create or alias durable pages only when source evidence supports them:\n- " + strings.Join(targets, "\n- "),
		}, affected); err != nil {
			return count, err
		}
	}
	return count, nil
}

func ensureSourceSummaryOutlinks(projectPath string, manifest SourceManifest) (int, error) {
	linked := 0
	for _, entry := range manifest.Sources {
		var summaryPath string
		var related []string
		for _, path := range entry.Files {
			data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(path)))
			if err != nil {
				continue
			}
			frontmatter, parseErr := parseGeneratedFrontmatter(string(data))
			if parseErr == nil && strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
				summaryPath = path
				continue
			}
			related = append(related, path)
		}
		if summaryPath == "" {
			continue
		}
		abs := filepath.Join(projectPath, filepath.FromSlash(summaryPath))
		data, err := os.ReadFile(abs)
		if err != nil {
			return linked, err
		}
		if strings.Contains(string(data), "[[") {
			continue
		}
		sort.Strings(related)
		var b strings.Builder
		b.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
		if len(related) == 0 {
			b.WriteString("\n## Wiki Navigation\n\n- [[index|Wiki index]]\n")
		} else {
			b.WriteString("\n## Related Wiki Pages\n\n")
			for _, path := range related {
				target := strings.TrimSuffix(filepath.Base(path), ".md")
				fmt.Fprintf(&b, "- [[%s]]\n", target)
			}
		}
		if err := wiki.WriteVersionedPage(projectPath, summaryPath, []byte(b.String()), "add deterministic source-summary navigation"); err != nil {
			return linked, err
		}
		linked++
	}
	return linked, nil
}

// ensureContentPageOutlinks gives a generated knowledge page a deterministic
// evidence-navigation edge when the model emitted no wikilinks. The target is
// an exact source-summary path from the durable manifest, so this repairs the
// structural invariant without inventing a semantic relationship.
func ensureContentPageOutlinks(projectPath string, manifest SourceManifest) (int, error) {
	pageSummaries := map[string]map[string]bool{}
	for _, entry := range manifest.Sources {
		var summaries []string
		var contentPages []string
		for _, path := range entry.Files {
			path = filepath.ToSlash(path)
			data, err := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(path)))
			if err != nil {
				continue
			}
			frontmatter, parseErr := parseGeneratedFrontmatter(string(data))
			if parseErr == nil && strings.TrimSpace(fmt.Sprint(frontmatter["type"])) == "source-summary" {
				summaries = append(summaries, path)
				continue
			}
			contentPages = append(contentPages, path)
		}
		for _, page := range contentPages {
			if pageSummaries[page] == nil {
				pageSummaries[page] = map[string]bool{}
			}
			for _, summary := range summaries {
				pageSummaries[page][summary] = true
			}
		}
	}

	linked := 0
	for page, summarySet := range pageSummaries {
		abs := filepath.Join(projectPath, filepath.FromSlash(page))
		data, err := os.ReadFile(abs)
		if err != nil {
			return linked, err
		}
		if generatedWikiLinkPattern.Match(data) || len(summarySet) == 0 {
			continue
		}
		summaries := make([]string, 0, len(summarySet))
		for summary := range summarySet {
			summaries = append(summaries, summary)
		}
		sort.Strings(summaries)
		var b strings.Builder
		b.Write(data)
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("\n## Source Pages\n\n")
		for _, summary := range summaries {
			target := strings.TrimSuffix(strings.TrimPrefix(summary, "wiki/"), ".md")
			fmt.Fprintf(&b, "- [[%s|Source summary]]\n", target)
		}
		if err := wiki.WriteVersionedPage(projectPath, page, []byte(b.String()), "add deterministic source provenance navigation"); err != nil {
			return linked, err
		}
		linked++
	}
	return linked, nil
}

func coalesceSemanticDuplicateGroups(groups map[string][]convergencePage, pathMap map[string]string) (map[string][]convergencePage, map[string]string) {
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parent := make([]int, len(keys))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(value int) int {
		if parent[value] != value {
			parent[value] = find(parent[value])
		}
		return parent[value]
	}
	union := func(a, b int) {
		a, b = find(a), find(b)
		if a != b {
			parent[b] = a
		}
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			leftType := canonicalTypeForPath(keys[i], "")
			rightType := canonicalTypeForPath(keys[j], "")
			crossTypeExact := leftType != rightType && convergenceCrossTypeExactTitleMergeAllowed(leftType, rightType) && convergenceGroupsHaveExactTitle(groups[keys[i]], groups[keys[j]])
			if leftType != rightType && !crossTypeExact {
				continue
			}
			if convergenceGroupsShareIdentity(groups[keys[i]], groups[keys[j]]) {
				union(i, j)
			}
		}
	}
	components := map[int][]string{}
	for index, key := range keys {
		root := find(index)
		components[root] = append(components[root], key)
	}
	destMap := map[string]string{}
	merged := map[string][]convergencePage{}
	for _, component := range components {
		canonical := component[0]
		canonicalWeight := convergenceGroupWeight(groups[canonical])
		for _, key := range component[1:] {
			weight := convergenceGroupWeight(groups[key])
			priority := convergenceCanonicalTypePriority(key)
			canonicalPriority := convergenceCanonicalTypePriority(canonical)
			if priority > canonicalPriority || priority == canonicalPriority && (weight > canonicalWeight || weight == canonicalWeight && key < canonical) {
				canonical = key
				canonicalWeight = weight
			}
		}
		for _, key := range component {
			destMap[key] = canonical
			merged[canonical] = append(merged[canonical], groups[key]...)
		}
	}
	for oldPath, dest := range pathMap {
		if canonical, ok := destMap[dest]; ok {
			pathMap[oldPath] = canonical
		}
	}
	return merged, pathMap
}

func convergenceGroupsHaveExactTitle(left, right []convergencePage) bool {
	leftTitles, _ := convergenceIdentityNames(left)
	rightTitles, _ := convergenceIdentityNames(right)
	for title := range leftTitles {
		if rightTitles[title] {
			return true
		}
	}
	return false
}

func convergenceCrossTypeExactTitleMergeAllowed(leftType, rightType string) bool {
	return leftType == "entity" && rightType == "concept" || leftType == "concept" && rightType == "entity"
}

func convergenceCanonicalTypePriority(path string) int {
	switch canonicalTypeForPath(path, "") {
	case "entity":
		return 2
	case "concept":
		return 1
	default:
		return 0
	}
}

func convergenceAmbiguousAliases(groups map[string][]convergencePage, sourceOwners map[string]map[string]bool) (map[string]bool, map[string]map[string]bool) {
	owners := map[string]map[string]bool{}
	for dest, pages := range groups {
		if len(sourceOwners[dest]) == 0 && canonicalTypeForPath(dest, "") != "synthesis" {
			continue
		}
		for _, page := range pages {
			identities := append([]string{page.title}, frontmatterStrings(page.fm["aliases"])...)
			for _, identity := range identities {
				identity = normalizeIdentityName(identity)
				if identity == "" {
					continue
				}
				if owners[identity] == nil {
					owners[identity] = map[string]bool{}
				}
				owners[identity][dest] = true
			}
		}
	}
	ambiguous := map[string]bool{}
	paths := map[string]map[string]bool{}
	for identity, identityOwners := range owners {
		if len(identityOwners) < 2 {
			continue
		}
		ambiguous[identity] = true
		paths[identity] = identityOwners
	}
	return ambiguous, paths
}

func convergenceGroupsShareIdentity(left, right []convergencePage) bool {
	leftTitles, leftAliases := convergenceIdentityNames(left)
	rightTitles, rightAliases := convergenceIdentityNames(right)
	for title := range leftTitles {
		if rightTitles[title] {
			return true
		}
	}
	leftRecognizesRight := false
	for title := range rightTitles {
		if leftAliases[title] {
			leftRecognizesRight = true
			break
		}
	}
	rightRecognizesLeft := false
	for title := range leftTitles {
		if rightAliases[title] {
			rightRecognizesLeft = true
			break
		}
	}
	if leftRecognizesRight && rightRecognizesLeft {
		return true
	}
	// Legacy compilers sometimes translated the canonical title differently
	// while preserving the same native-language identity set. Two shared
	// aliases, including at least one specific name of three or more runes, are
	// strong enough to coalesce without relying on corpus-specific knowledge.
	sharedAliases := 0
	hasSpecificSharedAlias := false
	for alias := range leftAliases {
		if !rightAliases[alias] {
			continue
		}
		sharedAliases++
		if len([]rune(strings.ReplaceAll(alias, " ", ""))) >= 3 {
			hasSpecificSharedAlias = true
		}
	}
	if sharedAliases >= 2 && hasSpecificSharedAlias {
		return true
	}
	return convergenceShortTitleRecognizesLongTitle(leftTitles, leftAliases, rightTitles, rightAliases)
}

// convergenceIdentityNames deliberately keeps titles and aliases separate.
// A single model-generated alias is not sufficient evidence to merge two wiki
// pages: aliases can be broad, ambiguous, or simply wrong. Different titles
// therefore merge only when both groups explicitly recognize each other as an
// alias. A one-way alias is accepted only for a full title containment such as
// "Sanzang" and "Tang Sanzang". Exact normalized titles remain safe to merge
// directly.
func convergenceIdentityNames(pages []convergencePage) (map[string]bool, map[string]bool) {
	titles := map[string]bool{}
	aliases := map[string]bool{}
	for _, page := range pages {
		title := normalizeIdentityName(page.title)
		if title != "" {
			titles[title] = true
		}
		identityAliases := append(frontmatterStrings(page.fm["aliases"]), frontmatterStrings(page.fm["ambiguous_aliases"])...)
		for _, alias := range identityAliases {
			alias = normalizeIdentityName(alias)
			if alias != "" {
				aliases[alias] = true
			}
		}
	}
	return titles, aliases
}

func convergenceShortTitleRecognizesLongTitle(leftTitles, leftAliases, rightTitles, rightAliases map[string]bool) bool {
	for leftTitle := range leftTitles {
		for rightTitle := range rightTitles {
			switch {
			case identityNameContains(rightTitle, leftTitle) && leftAliases[rightTitle]:
				return true
			case identityNameContains(leftTitle, rightTitle) && rightAliases[leftTitle]:
				return true
			}
		}
	}
	return false
}

func identityNameContains(longer, shorter string) bool {
	if longer == shorter || shorter == "" {
		return longer == shorter
	}
	if cjkIdentityName(shorter) {
		return len([]rune(shorter)) >= 2 && strings.Contains(longer, shorter)
	}
	if len([]rune(shorter)) < 4 {
		return false
	}
	return strings.Contains(" "+longer+" ", " "+shorter+" ")
}

func cjkIdentityName(value string) bool {
	for _, r := range value {
		if r >= '\u3400' && r <= '\u9fff' {
			return true
		}
	}
	return false
}

func normalizeIdentityName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '’' || r == '\'' {
			return ' '
		}
		return r
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func convergenceGroupWeight(pages []convergencePage) int {
	weight := 0
	for _, page := range pages {
		weight += len(page.body) + len(frontmatterStrings(page.fm["sources"]))*500
	}
	return weight
}

func convergenceTitleTokenOwners(groups map[string][]convergencePage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for dest, pages := range groups {
		for _, page := range pages {
			for _, token := range strings.Fields(normalizeIdentityName(page.title)) {
				if out[token] == nil {
					out[token] = map[string]bool{}
				}
				out[token][dest] = true
			}
		}
	}
	return out
}

func dropMisassignedAlias(alias, title, dest string, owners map[string]map[string]bool) bool {
	normalized := normalizeIdentityName(alias)
	if normalized == "" || strings.Contains(normalized, " ") || strings.Contains(normalizeIdentityName(title), normalized) {
		return false
	}
	for owner := range owners[normalized] {
		if owner != dest {
			return true
		}
	}
	return false
}

func canonicalTypeForPath(path, fallback string) string {
	switch {
	case strings.HasPrefix(path, "wiki/sources/"):
		return "source-summary"
	case strings.HasPrefix(path, "wiki/entities/"):
		return "entity"
	case strings.HasPrefix(path, "wiki/concepts/"):
		return "concept"
	case strings.HasPrefix(path, "wiki/syntheses/"):
		return "synthesis"
	}
	return fallback
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func stripLeadingH1(body string) string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		lines = lines[1:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func rewriteCanonicalWikiLinks(projectPath string, pathMap map[string]string) error {
	replacements := map[string]string{}
	for oldPath, newPath := range pathMap {
		if newPath == "" || oldPath == newPath {
			continue
		}
		oldNoExt := strings.TrimSuffix(strings.TrimPrefix(oldPath, "wiki/"), ".md")
		newNoExt := strings.TrimSuffix(strings.TrimPrefix(newPath, "wiki/"), ".md")
		replacements["[["+oldPath] = "[[" + newNoExt
		replacements["[["+strings.TrimSuffix(oldPath, ".md")] = "[[" + newNoExt
		replacements["[["+oldNoExt] = "[[" + newNoExt
	}
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return err
	}
	for _, page := range pages {
		abs := filepath.Join(projectPath, filepath.FromSlash(page.Path))
		data, readErr := os.ReadFile(abs)
		if readErr != nil {
			return readErr
		}
		updated := string(data)
		for oldValue, newValue := range replacements {
			updated = strings.ReplaceAll(updated, oldValue, newValue)
		}
		if updated != string(data) {
			if err := wiki.WriteVersionedPage(projectPath, page.Path, []byte(updated), "rewrite links to canonical wiki paths"); err != nil {
				return err
			}
		}
	}
	return nil
}

var reviewHeadingPattern = regexp.MustCompile(`(?m)^(## \[[^]]+\] )([^|]+)( \|)`)

func normalizeReviewTypes(projectPath string) (int, error) {
	path := filepath.Join(projectPath, "wiki", "reviews.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	updated := reviewHeadingPattern.ReplaceAllStringFunc(string(data), func(line string) string {
		parts := reviewHeadingPattern.FindStringSubmatch(line)
		if len(parts) != 4 {
			return line
		}
		normalized := canonicalReviewType(parts[2])
		if normalized == strings.TrimSpace(parts[2]) {
			return line
		}
		count++
		return parts[1] + normalized + parts[3]
	})
	if updated != string(data) {
		if err := wiki.WriteVersionedPage(projectPath, "wiki/reviews.md", []byte(updated), "normalize review taxonomy"); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func canonicalReviewType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "missing") || strings.Contains(value, "unresolved"):
		return "missing-page"
	case strings.Contains(value, "duplicate") || strings.Contains(value, "merge") || strings.Contains(value, "identity"):
		return "duplicate"
	case strings.Contains(value, "contradict") || strings.Contains(value, "conflict"):
		return "contradiction"
	case strings.Contains(value, "stale") || strings.Contains(value, "outdated"):
		return "stale-claim"
	case strings.Contains(value, "source") || strings.Contains(value, "provenance") || strings.Contains(value, "evidence gap"):
		return "source-gap"
	default:
		return "review-needed"
	}
}

func quarantineUnownedWikiPages(projectPath string, manifest SourceManifest) (int, error) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/log.md" || page.Path == "wiki/reviews.md" || page.Type == "synthesis" {
			continue
		}
		owner, tracked := manifest.PageOwners[page.Path]
		// Unknown, manual, review, query and code pages are durable. Only pages
		// explicitly marked source-managed can become compiler orphans.
		if !tracked || owner.ManagedBy != "source" || pageOwnedByActiveSource(page.Path, owner, manifest) {
			continue
		}
		if err := wiki.QuarantineVersionedPage(projectPath, page.Path, "manifest ownership reconciliation"); err != nil {
			return count, err
		}
		delete(manifest.PageOwners, page.Path)
		count++
	}
	if count > 0 {
		if err := saveSourceManifest(projectPath, manifest); err != nil {
			return count, err
		}
	}
	return count, nil
}

func countUnownedWikiPages(projectPath string, manifest SourceManifest) (int, error) {
	pages, err := wiki.ScanWikiPages(wiki.ScanOptions{ProjectPath: projectPath})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, page := range pages {
		if page.Path == "wiki/index.md" || page.Path == "wiki/overview.md" || page.Path == "wiki/log.md" || page.Path == "wiki/reviews.md" || page.Type == "synthesis" {
			continue
		}
		owner, tracked := manifest.PageOwners[page.Path]
		if tracked && owner.ManagedBy == "source" && !pageOwnedByActiveSource(page.Path, owner, manifest) {
			count++
		}
	}
	return count, nil
}

func pageOwnedByActiveSource(pagePath string, owner manifestfile.PageOwnership, manifest SourceManifest) bool {
	pagePath = filepath.ToSlash(pagePath)
	for _, key := range owner.SourceKeys {
		entry, ok := manifest.Sources[key]
		if !ok {
			continue
		}
		for _, path := range manifestfile.ActiveOwnedFiles(entry) {
			if filepath.ToSlash(path) == pagePath {
				return true
			}
		}
	}
	return false
}
