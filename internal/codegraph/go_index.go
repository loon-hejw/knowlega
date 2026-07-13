package codegraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hejw/knowledge-core/internal/core"
)

type GoIndexOptions struct {
	RepoID       string
	RepoPath     string
	Commit       string
	IgnoreDirs   []string
	IncludeTests bool
}

type parsedGoFile struct {
	AST        *ast.File
	FSet       *token.FileSet
	AbsPath    string
	RelPath    string
	PackageKey string
	PackageID  string
	FileID     string
	Imports    map[string]string
}

type goGraphBuilder struct {
	repoID          string
	repoPath        string
	nodes           map[string]Node
	edges           map[string]Edge
	packages        map[string]string
	functions       map[string]string
	methods         map[string]string
	types           map[string]string
	methodSets      map[string]map[string]string
	interfaces      map[string]map[string]bool
	interfaceEmbeds map[string][]string
	pendingEmbeds   []pendingEmbed
	parsed          []parsedGoFile
	sourceSHA256    string
}

type pendingEmbed struct {
	SourceID   string
	TypeKey    string
	Label      string
	SourceFile string
	Evidence   string
}

func IndexGoRepository(opts GoIndexOptions) (Snapshot, error) {
	if strings.TrimSpace(opts.RepoPath) == "" {
		return Snapshot{}, fmt.Errorf("repo path is required")
	}
	repoPath, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return Snapshot{}, err
	}
	if opts.RepoID == "" {
		opts.RepoID = core.Slug(filepath.Base(repoPath))
	}
	opts.RepoID = strings.TrimSpace(opts.RepoID)
	if opts.RepoID == "" || opts.RepoID == "." || opts.RepoID == ".." || filepath.IsAbs(opts.RepoID) || strings.ContainsAny(opts.RepoID, "/\\\x00") {
		return Snapshot{}, fmt.Errorf("repo id must be a safe path segment")
	}
	info, err := os.Stat(repoPath)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() {
		return Snapshot{}, fmt.Errorf("repo path is not a directory: %s", repoPath)
	}
	b := &goGraphBuilder{
		repoID: opts.RepoID, repoPath: repoPath,
		nodes: map[string]Node{}, edges: map[string]Edge{}, packages: map[string]string{},
		functions: map[string]string{}, methods: map[string]string{}, types: map[string]string{},
		methodSets: map[string]map[string]string{}, interfaces: map[string]map[string]bool{}, interfaceEmbeds: map[string][]string{},
	}
	if err := b.parseRepository(opts); err != nil {
		return Snapshot{}, err
	}
	b.indexDeclarations()
	b.indexImportsCallsAndRoutes()
	b.indexImplementations()

	snapshot := Snapshot{
		Version: 1, RepoID: opts.RepoID, RepoPath: repoPath, Commit: strings.TrimSpace(opts.Commit),
		Source: "go-native", ImportedAt: time.Now().UTC(), Nodes: b.sortedNodes(), Edges: b.sortedEdges(),
	}
	if snapshot.Commit == "" {
		snapshot.Commit = gitHead(repoPath)
	}
	snapshot.Dirty = gitDirty(repoPath)
	snapshot.SourceSHA256 = b.sourceSHA256
	ApplyCommunities(&snapshot)
	DetectProcesses(&snapshot, 10, 4, 3, 75)
	BuildSnapshotInsights(&snapshot)
	snapshot.ReportTitle = "GRAPH_REPORT.md"
	snapshot.ReportBody = RenderGraphReport(snapshot)
	return snapshot, nil
}

func (b *goGraphBuilder) parseRepository(opts GoIndexOptions) error {
	ignored := map[string]bool{
		".git": true, ".kbcore": true, "vendor": true, "node_modules": true,
		"dist": true, "build": true, "testdata": true,
	}
	for _, value := range opts.IgnoreDirs {
		ignored[strings.TrimSpace(value)] = true
	}
	var files []string
	err := filepath.WalkDir(b.repoPath, func(filename string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if filename != b.repoPath && ignored[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".go") {
			return nil
		}
		if !opts.IncludeTests && strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		files = append(files, filename)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(files)
	sourceHash := sha256.New()
	rootFolderID := b.addNode(NodeFolder, b.repoID, "", map[string]any{"path": "."})
	_ = rootFolderID
	for _, filename := range files {
		data, err := os.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("read %s: %w", filename, err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, filename, data, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", filename, err)
		}
		rel, err := filepath.Rel(b.repoPath, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		_, _ = sourceHash.Write([]byte(rel))
		_, _ = sourceHash.Write([]byte{0})
		_, _ = sourceHash.Write(data)
		_, _ = sourceHash.Write([]byte{0})
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "." {
			dir = ""
		}
		folderID := b.ensureFolder(dir)
		packageKey := dir + "|" + parsed.Name.Name
		packageID := b.packages[packageKey]
		if packageID == "" {
			label := parsed.Name.Name
			if dir != "" {
				label = dir + "/" + label
			}
			packageID = b.addNode(NodePackage, label, rel, map[string]any{"package": parsed.Name.Name, "directory": dir})
			b.packages[packageKey] = packageID
			b.addEdge(folderID, packageID, RelContains, "EXTRACTED", 1, rel, nil)
		}
		fileID := b.addNode(NodeFile, filepath.Base(rel), rel, map[string]any{"path": rel, "package": parsed.Name.Name})
		b.addEdge(folderID, fileID, RelContains, "EXTRACTED", 1, rel, nil)
		b.addEdge(packageID, fileID, RelContains, "EXTRACTED", 1, rel, nil)
		imports := map[string]string{}
		for _, spec := range parsed.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			alias := filepath.Base(importPath)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias != "_" && alias != "." {
				imports[alias] = importPath
			}
		}
		b.parsed = append(b.parsed, parsedGoFile{AST: parsed, FSet: fset, AbsPath: filename, RelPath: rel, PackageKey: packageKey, PackageID: packageID, FileID: fileID, Imports: imports})
	}
	b.sourceSHA256 = hex.EncodeToString(sourceHash.Sum(nil))
	return nil
}

func (b *goGraphBuilder) indexDeclarations() {
	for _, file := range b.parsed {
		pkgName := file.AST.Name.Name
		for _, decl := range file.AST.Decls {
			switch value := decl.(type) {
			case *ast.FuncDecl:
				receiver := receiverType(value.Recv)
				kind := NodeFunction
				qualified := pkgName + "." + value.Name.Name
				if receiver != "" {
					kind = NodeMethod
					qualified = pkgName + "." + receiver + "." + value.Name.Name
				}
				props := b.declarationProps(file, value.Pos(), value.End(), value.Doc, qualified)
				props["signature"] = renderAST(file.FSet, value.Type)
				props["exported"] = ast.IsExported(value.Name.Name)
				if receiver != "" {
					props["receiver"] = receiver
				}
				id := b.addNode(kind, qualified, file.RelPath, props)
				b.addEdge(file.FileID, id, RelDefines, "EXTRACTED", 1, evidenceAt(file, value.Pos()), nil)
				if receiver == "" {
					b.functions[file.PackageKey+"|"+value.Name.Name] = id
				} else {
					methodKey := file.PackageKey + "|" + receiver + "|" + value.Name.Name
					b.methods[methodKey] = id
					if b.methodSets[file.PackageKey+"|"+receiver] == nil {
						b.methodSets[file.PackageKey+"|"+receiver] = map[string]string{}
					}
					b.methodSets[file.PackageKey+"|"+receiver][value.Name.Name] = id
				}
			case *ast.GenDecl:
				b.indexGenDecl(file, value)
			}
		}
	}
	for methodKey, methodID := range b.methods {
		parts := strings.Split(methodKey, "|")
		if len(parts) < 4 {
			continue
		}
		typeKey := strings.Join(parts[:2], "|") + "|" + parts[2]
		if typeID := b.types[typeKey]; typeID != "" {
			b.addEdge(typeID, methodID, RelHasMethod, "EXTRACTED", 1, b.nodes[methodID].SourceFile, nil)
		}
	}
	for _, pending := range b.pendingEmbeds {
		targetID := b.types[pending.TypeKey]
		if targetID == "" {
			targetID = b.addNode(NodeType, pending.Label, pending.SourceFile, map[string]any{"external": true})
		}
		b.addEdge(pending.SourceID, targetID, RelEmbeds, "EXTRACTED", 1, pending.Evidence, nil)
	}
}

func (b *goGraphBuilder) indexGenDecl(file parsedGoFile, decl *ast.GenDecl) {
	pkgName := file.AST.Name.Name
	for _, rawSpec := range decl.Specs {
		switch spec := rawSpec.(type) {
		case *ast.TypeSpec:
			kind := NodeType
			switch spec.Type.(type) {
			case *ast.StructType:
				kind = NodeStruct
			case *ast.InterfaceType:
				kind = NodeInterface
			}
			qualified := pkgName + "." + spec.Name.Name
			props := b.declarationProps(file, spec.Pos(), spec.End(), decl.Doc, qualified)
			props["definition"] = renderAST(file.FSet, spec.Type)
			props["exported"] = ast.IsExported(spec.Name.Name)
			id := b.addNode(kind, qualified, file.RelPath, props)
			b.types[file.PackageKey+"|"+spec.Name.Name] = id
			b.addEdge(file.FileID, id, RelDefines, "EXTRACTED", 1, evidenceAt(file, spec.Pos()), nil)
			switch typed := spec.Type.(type) {
			case *ast.StructType:
				for _, field := range typed.Fields.List {
					if len(field.Names) != 0 {
						continue
					}
					embedded := expressionTypeName(field.Type)
					if embedded != "" {
						typeKey := ""
						if !strings.Contains(embedded, ".") {
							typeKey = file.PackageKey + "|" + embedded
						}
						b.pendingEmbeds = append(b.pendingEmbeds, pendingEmbed{SourceID: id, TypeKey: typeKey, Label: embedded, SourceFile: file.RelPath, Evidence: evidenceAt(file, field.Pos())})
					}
				}
			case *ast.InterfaceType:
				required := map[string]bool{}
				interfaceKey := file.PackageKey + "|" + spec.Name.Name
				for _, method := range typed.Methods.List {
					if len(method.Names) == 0 {
						embedded := expressionTypeName(method.Type)
						if embedded != "" {
							typeKey := ""
							if !strings.Contains(embedded, ".") {
								typeKey = file.PackageKey + "|" + embedded
								b.interfaceEmbeds[interfaceKey] = append(b.interfaceEmbeds[interfaceKey], typeKey)
							}
							b.pendingEmbeds = append(b.pendingEmbeds, pendingEmbed{SourceID: id, TypeKey: typeKey, Label: embedded, SourceFile: file.RelPath, Evidence: evidenceAt(file, method.Pos())})
						}
					}
					for _, name := range method.Names {
						required[name.Name] = true
						methodQualified := qualified + "." + name.Name
						methodID := b.addNode(NodeMethod, methodQualified, file.RelPath, map[string]any{
							"qualified_name": methodQualified, "signature": renderAST(file.FSet, method.Type), "interface_method": true,
						})
						b.addEdge(id, methodID, RelHasMethod, "EXTRACTED", 1, evidenceAt(file, method.Pos()), nil)
					}
				}
				b.interfaces[interfaceKey] = required
			}
		case *ast.ValueSpec:
			kind := NodeVariable
			if decl.Tok == token.CONST {
				kind = NodeConst
			}
			for _, name := range spec.Names {
				qualified := pkgName + "." + name.Name
				id := b.addNode(kind, qualified, file.RelPath, b.declarationProps(file, name.Pos(), spec.End(), decl.Doc, qualified))
				b.addEdge(file.FileID, id, RelDefines, "EXTRACTED", 1, evidenceAt(file, name.Pos()), nil)
			}
		}
	}
}

func (b *goGraphBuilder) indexImportsCallsAndRoutes() {
	for _, file := range b.parsed {
		for alias, importPath := range file.Imports {
			packageID := b.addNode(NodePackage, importPath, file.RelPath, map[string]any{"import_path": importPath, "external": true, "alias": alias})
			b.addEdge(file.FileID, packageID, RelImports, "EXTRACTED", 1, file.RelPath, nil)
		}
		for _, decl := range file.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			currentID := b.functionDeclID(file, fn)
			if currentID == "" {
				continue
			}
			receiver := receiverType(fn.Recv)
			receiverVar := receiverVariable(fn.Recv)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if targetID, confidence, score := b.resolveCall(file, call.Fun, receiver, receiverVar); targetID != "" {
					b.addEdge(currentID, targetID, RelCalls, confidence, score, evidenceAt(file, call.Pos()), nil)
				}
				b.indexRouteCall(file, currentID, call)
				return true
			})
		}
	}
}

func (b *goGraphBuilder) resolveCall(file parsedGoFile, expression ast.Expr, receiver, receiverVar string) (string, string, float64) {
	switch called := expression.(type) {
	case *ast.Ident:
		if called.Obj != nil && called.Obj.Kind != ast.Fun {
			return "", "", 0
		}
		return b.functions[file.PackageKey+"|"+called.Name], "EXTRACTED", 1
	case *ast.SelectorExpr:
		owner, ok := called.X.(*ast.Ident)
		if !ok {
			return "", "", 0
		}
		if owner.Name == receiverVar && receiver != "" {
			return b.methods[file.PackageKey+"|"+receiver+"|"+called.Sel.Name], "EXTRACTED", 1
		}
		if importPath := file.Imports[owner.Name]; importPath != "" && (owner.Obj == nil || owner.Obj.Kind == ast.Pkg) {
			qualified := importPath + "." + called.Sel.Name
			id := b.addNode(NodeFunction, qualified, file.RelPath, map[string]any{"external": true, "import_path": importPath, "qualified_name": qualified})
			return id, "EXTRACTED", 1
		}
		if id := b.methods[file.PackageKey+"|"+owner.Name+"|"+called.Sel.Name]; id != "" {
			return id, "EXTRACTED", 1
		}
		var candidate string
		for key, id := range b.methods {
			if strings.HasPrefix(key, file.PackageKey+"|") && strings.HasSuffix(key, "|"+called.Sel.Name) {
				if candidate != "" && candidate != id {
					return "", "", 0
				}
				candidate = id
			}
		}
		if candidate != "" {
			return candidate, "INFERRED", 0.65
		}
	}
	return "", "", 0
}

func (b *goGraphBuilder) indexRouteCall(file parsedGoFile, currentID string, call *ast.CallExpr) {
	name := calledName(call.Fun)
	method := ""
	switch strings.ToUpper(name) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "HEAD":
		method = strings.ToUpper(name)
	case "HANDLE", "HANDLEFUNC", "HANDLEFUNCTION":
		method = "ANY"
	default:
		return
	}
	if len(call.Args) == 0 {
		return
	}
	pattern, ok := stringLiteral(call.Args[0])
	if !ok || !strings.Contains(pattern, "/") {
		return
	}
	if method == "ANY" {
		parts := strings.Fields(pattern)
		if len(parts) == 2 && strings.HasPrefix(parts[1], "/") {
			method, pattern = strings.ToUpper(parts[0]), parts[1]
		}
	}
	label := strings.TrimSpace(method + " " + pattern)
	routeID := b.addNode(NodeRoute, label, file.RelPath, map[string]any{"method": method, "path": pattern})
	handlerID := currentID
	if len(call.Args) > 1 {
		if resolved, _, _ := b.resolveCall(file, call.Args[1], "", ""); resolved != "" {
			handlerID = resolved
		}
	}
	b.addEdge(handlerID, routeID, RelHandlesRoute, "EXTRACTED", 1, evidenceAt(file, call.Pos()), nil)
	if strings.Contains(strings.ToLower(pattern), "/tools/") || strings.Contains(strings.ToLower(pattern), "/tool/") {
		toolID := b.addNode(NodeTool, label, file.RelPath, map[string]any{"route": pattern, "method": method})
		b.addEdge(handlerID, toolID, RelHandlesTool, "EXTRACTED", 1, evidenceAt(file, call.Pos()), nil)
	}
}

func (b *goGraphBuilder) indexImplementations() {
	for interfaceKey := range b.interfaces {
		required := b.interfaceMethodSet(interfaceKey, map[string]bool{})
		if len(required) == 0 {
			continue
		}
		interfaceID := b.types[interfaceKey]
		packagePrefix := interfaceKey[:strings.LastIndex(interfaceKey, "|")+1]
		for typeKey, methods := range b.methodSets {
			if !strings.HasPrefix(typeKey, packagePrefix) || typeKey == interfaceKey {
				continue
			}
			implements := true
			for method := range required {
				if methods[method] == "" {
					implements = false
					break
				}
			}
			if implements {
				if typeID := b.types[typeKey]; typeID != "" {
					b.addEdge(typeID, interfaceID, RelImplements, "EXTRACTED", 1, b.nodes[typeID].SourceFile, map[string]any{"method_count": len(required)})
				}
			}
		}
	}
}

func (b *goGraphBuilder) interfaceMethodSet(interfaceKey string, visiting map[string]bool) map[string]bool {
	out := map[string]bool{}
	if visiting[interfaceKey] {
		return out
	}
	visiting[interfaceKey] = true
	for method := range b.interfaces[interfaceKey] {
		out[method] = true
	}
	for _, embedded := range b.interfaceEmbeds[interfaceKey] {
		for method := range b.interfaceMethodSet(embedded, visiting) {
			out[method] = true
		}
	}
	delete(visiting, interfaceKey)
	return out
}

func (b *goGraphBuilder) functionDeclID(file parsedGoFile, fn *ast.FuncDecl) string {
	receiver := receiverType(fn.Recv)
	if receiver == "" {
		return b.functions[file.PackageKey+"|"+fn.Name.Name]
	}
	return b.methods[file.PackageKey+"|"+receiver+"|"+fn.Name.Name]
}

func (b *goGraphBuilder) declarationProps(file parsedGoFile, start, end token.Pos, doc *ast.CommentGroup, qualified string) map[string]any {
	startPos := file.FSet.Position(start)
	endPos := file.FSet.Position(end)
	props := map[string]any{
		"qualified_name": qualified, "path": file.RelPath, "line": startPos.Line, "end_line": endPos.Line,
		"package": file.AST.Name.Name,
	}
	if doc != nil && strings.TrimSpace(doc.Text()) != "" {
		props["doc"] = strings.TrimSpace(doc.Text())
	}
	return props
}

func (b *goGraphBuilder) ensureFolder(dir string) string {
	if dir == "" || dir == "." {
		for id, node := range b.nodes {
			if node.Kind == NodeFolder && node.Props["path"] == "." {
				return id
			}
		}
		return b.addNode(NodeFolder, b.repoID, "", map[string]any{"path": "."})
	}
	parts := strings.Split(filepath.ToSlash(dir), "/")
	parent := b.ensureFolder("")
	current := ""
	for _, part := range parts {
		current = strings.Trim(current+"/"+part, "/")
		id := b.nodeID(NodeFolder, current)
		if _, ok := b.nodes[id]; !ok {
			id = b.addNode(NodeFolder, part, current, map[string]any{"path": current})
			b.addEdge(parent, id, RelContains, "EXTRACTED", 1, current, nil)
		}
		parent = id
	}
	return parent
}

func (b *goGraphBuilder) addNode(kind NodeKind, label, sourceFile string, props map[string]any) string {
	key := label
	if kind == NodeFile || kind == NodeFolder {
		key = firstNonEmpty(sourceFile, label)
	}
	if kind == NodePackage && props != nil {
		importPath, _ := props["import_path"].(string)
		directory, _ := props["directory"].(string)
		packageName, _ := props["package"].(string)
		key = firstNonEmpty(importPath, directory+"|"+packageName, label)
	}
	id := b.nodeID(kind, key)
	if existing, ok := b.nodes[id]; ok {
		if existing.Props == nil {
			existing.Props = map[string]any{}
		}
		for key, value := range props {
			existing.Props[key] = value
		}
		b.nodes[id] = existing
		return id
	}
	if props == nil {
		props = map[string]any{}
	}
	b.nodes[id] = Node{ID: id, Kind: kind, Label: label, SourceFile: filepath.ToSlash(sourceFile), Props: props}
	return id
}

func (b *goGraphBuilder) nodeID(kind NodeKind, key string) string {
	return core.StableID(b.repoID, "go-native", string(kind), filepath.ToSlash(key))
}

func (b *goGraphBuilder) addEdge(source, target string, relation Relation, confidence string, score float64, evidence string, props map[string]any) {
	if source == "" || target == "" || source == target {
		return
	}
	key := source + "\x00" + target + "\x00" + string(relation)
	if existing, ok := b.edges[key]; ok {
		if evidence != "" && !containsString(existing.Evidence, evidence) {
			existing.Evidence = append(existing.Evidence, evidence)
		}
		existing.Weight++
		b.edges[key] = existing
		return
	}
	if score <= 0 {
		score = 1
	}
	if props == nil {
		props = map[string]any{}
	}
	edge := Edge{Source: source, Target: target, Relation: relation, Confidence: confidence, ConfidenceScore: score, Weight: 1, Props: props}
	if evidence != "" {
		edge.Evidence = []string{filepath.ToSlash(evidence)}
	}
	b.edges[key] = edge
}

func (b *goGraphBuilder) sortedNodes() []Node {
	ids := make([]string, 0, len(b.nodes))
	for id := range b.nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Node, 0, len(ids))
	for _, id := range ids {
		out = append(out, b.nodes[id])
	}
	return out
}

func (b *goGraphBuilder) sortedEdges() []Edge {
	keys := make([]string, 0, len(b.edges))
	for key := range b.edges {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]Edge, 0, len(keys))
	for _, key := range keys {
		out = append(out, b.edges[key])
	}
	return out
}

func receiverType(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 {
		return ""
	}
	return expressionTypeName(fields.List[0].Type)
}

func receiverVariable(fields *ast.FieldList) string {
	if fields == nil || len(fields.List) == 0 || len(fields.List[0].Names) == 0 {
		return ""
	}
	return fields.List[0].Names[0].Name
}

func expressionTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return expressionTypeName(value.X)
	case *ast.SelectorExpr:
		return expressionTypeName(value.X) + "." + value.Sel.Name
	case *ast.IndexExpr:
		return expressionTypeName(value.X)
	case *ast.IndexListExpr:
		return expressionTypeName(value.X)
	default:
		return ""
	}
}

func renderAST(fset *token.FileSet, node any) string {
	astNode, ok := node.(ast.Node)
	if !ok || astNode == nil {
		return ""
	}
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fset, astNode); err != nil {
		return ""
	}
	return buffer.String()
}

func calledName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func stringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func evidenceAt(file parsedGoFile, position token.Pos) string {
	return fmt.Sprintf("%s:%d", file.RelPath, file.FSet.Position(position).Line)
}

func gitHead(repoPath string) string {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitDirty(repoPath string) bool {
	cmd := exec.Command("git", "-C", repoPath, "status", "--porcelain=v1", "--untracked-files=all")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) >= 3 && strings.TrimSpace(line[3:]) == ".kbcore-managed-repo.json" {
			continue
		}
		return true
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
