package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/carlverge/jsonnet-lsp/pkg/analysis"
	"github.com/carlverge/jsonnet-lsp/pkg/overlay"
	"github.com/google/go-jsonnet"
	"github.com/google/go-jsonnet/ast"
	"github.com/google/go-jsonnet/linter"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/span"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func logf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "I%s]%s\n", time.Now().Format("0201 15:04:05.00000"), fmt.Sprintf(msg, args...))
}

type Server struct {
	*FallbackServer

	rootURI     uri.URI
	rootFS      fs.FS
	searchPaths []string

	overlay *overlay.Overlay
	vmlock  sync.Mutex

	// intentionally only keep one active VM at once
	// when an operation needs a full VM (f.ex if it needs to
	// traverse imports) then dump the VM and create a new one.
	// This usually only happens when users switch and then edit a file,
	// and the latency is usually on the order of <1s. Not acceptable on
	// every operation, but acceptable on file change. This helps keep
	// memory usage low as we don't keep a VM in memory for every active
	// file we're editing.
	vm *vmCache

	cancel   context.CancelFunc
	notifier protocol.Client
}

type readCloser struct {
	io.ReadCloser
	io.Writer
}

func RunServer(ctx context.Context, stdout *os.File) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn := &readCloser{os.Stdin, stdout}

	logger := protocol.LoggerFromContext(ctx)
	logger.Debug("running in stdio mode")
	stream := jsonrpc2.NewStream(conn)
	jsonConn := jsonrpc2.NewConn(stream)
	notifier := protocol.ClientDispatcher(jsonConn, logger.Named("notify"))

	srv := &Server{
		FallbackServer: &FallbackServer{},
		overlay:        overlay.NewOverlay(),
		cancel:         cancel,
		notifier:       notifier,
	}

	handler := srv.Handler()
	jsonConn.Go(ctx, handler)

	select {
	case <-ctx.Done():
		_ = jsonConn.Close()
		return ctx.Err()
	case <-jsonConn.Done():
		if ctx.Err() == nil {
			if errors.Unwrap(jsonConn.Err()) != io.EOF {
				// only propagate connection error if context is still valid
				return jsonConn.Err()
			}
		}
	}

	return nil
}

func findRootDirectory(params *protocol.InitializeParams) uri.URI {
	for _, f := range params.WorkspaceFolders {
		return uri.URI(f.URI)
	}
	if params.RootURI != "" {
		return params.RootURI
	}
	if params.RootPath != "" {
		return uri.File(params.RootPath)
	}
	cwd, _ := os.Getwd()
	return uri.File(cwd)
}

func (s *Server) readURI(uri uri.URI) ([]byte, error) {
	// check overlay first -- use parsed as an unparsable result is not useful
	if ent := s.overlay.Parsed(uri); ent != nil {
		return []byte(ent.Contents), nil
	}

	path, err := filepath.Rel(s.rootURI.Filename(), uri.Filename())
	if err != nil {
		return nil, fmt.Errorf("failed to open URI '%s': %v", uri, err)
	}
	return fs.ReadFile(s.rootFS, path)
}

type lspImporter func(importedFrom, importedPath string) (contents jsonnet.Contents, foundAt string, err error)

func (l lspImporter) Import(importedFrom, importedPath string) (contents jsonnet.Contents, foundAt string, err error) {
	return l(importedFrom, importedPath)
}

type foundCacheItem struct {
	err      error
	contents jsonnet.Contents
	uri      uri.URI
}

func (s *Server) newImporter(from uri.URI) jsonnet.Importer {
	// assumption: a single importer will not be called concurrently, as the VM
	// it belongs to much be synchronized regardless
	cache := map[string]*foundCacheItem{}
	return lspImporter(func(importedFrom, importedPath string) (contents jsonnet.Contents, foundAt string, err error) {
		if item, ok := cache[importedPath]; ok {
			if item.err != nil {
				return jsonnet.Contents{}, "", item.err
			}
			return item.contents, item.uri.Filename(), nil
		}
		data, foundURI, err := s.readPath(importedPath, from)
		if err != nil {
			// add a negative cache entry
			cache[importedPath] = &foundCacheItem{err: err}
			return jsonnet.Contents{}, "", err
		}
		contents = jsonnet.MakeContentsRaw(data)
		cache[importedPath] = &foundCacheItem{contents: contents, uri: foundURI}
		return contents, foundURI.Filename(), nil
	})
}

// readPath will read an import path
func (s *Server) readPath(path string, from uri.URI) ([]byte, uri.URI, error) {
	rootPath := s.rootURI.Filename()

	// if absolute, rel it to the workspace root
	if filepath.IsAbs(path) {
		path, _ = filepath.Rel(rootPath, path)
	}

	// the path to the importer, relative to the root
	fromPath, err := filepath.Rel(rootPath, filepath.Dir(from.Filename()))
	if err != nil {
		return nil, "", fmt.Errorf("failed to open '%s' -- could not relativize '%s' to root '%s' %v", path, from, s.rootURI, err)
	}

	// Build a list of candidate URIs to try for the file
	candidates := []uri.URI{
		uri.File(filepath.Join(rootPath, path)),
		uri.File(filepath.Join(rootPath, fromPath, path)),
	}
	for _, search := range s.searchPaths {
		candidates = append(candidates, uri.File(filepath.Join(rootPath, search, path)))
	}

	// logf("searching for path '%s' in candidates %v", path, candidates)
	for _, candidate := range candidates {
		data, err := s.readURI(candidate)
		if err == nil {
			return data, candidate, nil
		}
	}
	return nil, "", fmt.Errorf("path '%s' not found in candidates %v", path, candidates)
}

func posToProto(p ast.Location) protocol.Position {
	line, col := p.Line, p.Column
	if line > 0 {
		line--
	}
	if col > 0 {
		col--
	}
	return protocol.Position{Line: uint32(line), Character: uint32(col)}
}

func protoToPos(p protocol.Position) ast.Location {
	return ast.Location{Line: int(p.Line) + 1, Column: int(p.Character) + 1}
}

func rangeToProto(r ast.LocationRange) protocol.Range {
	return protocol.Range{Start: posToProto(r.Begin), End: posToProto(r.End)}
}

type ErrCollector struct {
	URI   uri.URI
	Diags []protocol.Diagnostic
}

func (e *ErrCollector) Format(err error) string {
	e.Collect(err, protocol.DiagnosticSeverityWarning)
	return err.Error()
}

// staticError shadows the staticError internal interface in go-jsonnet/internal/errors
type staticError interface {
	Error() string
	Loc() ast.LocationRange
}

func (e *ErrCollector) Collect(err error, severity protocol.DiagnosticSeverity) {
	switch err := err.(type) {
	case staticError:
		if err.Loc().FileName == e.URI.Filename() {
			e.Diags = append(e.Diags, protocol.Diagnostic{
				Severity: severity,
				Range:    rangeToProto(err.Loc()),
				Message:  err.Error(),
				Source:   "jsonnet",
			})
		}
	case jsonnet.RuntimeError:
		e.Diags = append(e.Diags, protocol.Diagnostic{
			Range:    rangeToProto(err.StackTrace[0].Loc),
			Severity: protocol.DiagnosticSeverityError,
			Source:   "jsonnet",
			Message:  err.Msg,
		})
	default:
		logf("unknown lint error type (%T): %+v", err, err)
	}

}
func (e *ErrCollector) SetMaxStackTraceSize(size int)                  {}
func (e *ErrCollector) SetColorFormatter(color jsonnet.ColorFormatter) {}

type ErrDiscard struct{}

func (e ErrDiscard) Format(err error) string                        { return err.Error() }
func (e ErrDiscard) SetMaxStackTraceSize(size int)                  {}
func (e ErrDiscard) SetColorFormatter(color jsonnet.ColorFormatter) {}

type vmCache struct {
	lock sync.Mutex
	// from is the file that created the VM
	from uri.URI
	vm   *jsonnet.VM
}

func (c *vmCache) Use(fn func(vm *jsonnet.VM)) {
	c.lock.Lock()
	defer c.lock.Unlock()
	fn(c.vm)
}

func (c *vmCache) ImportAST(path string) (ast.Node, uri.URI) {
	c.lock.Lock()
	defer c.lock.Unlock()
	contents, foundAt, err := c.vm.ImportAST("", path)
	if err != nil {
		return nil, uri.URI("")
	}
	return contents, uri.File(foundAt)
}

func (s *Server) getVM(uri uri.URI) *vmCache {
	s.vmlock.Lock()
	defer s.vmlock.Unlock()

	// still on the same file, keep the vm cache
	if s.vm != nil && uri == s.vm.from {
		return s.vm
	}

	logf("flusing jsonnet vm cache (changed file to %s)", uri)
	vm := &vmCache{from: uri, vm: jsonnet.MakeVM()}
	vm.vm.Importer(s.newImporter(uri))
	vm.vm.SetTraceOut(io.Discard)
	s.vm = vm

	return vm
}

func convChangeEvents(events []protocol.TextDocumentContentChangeEvent) []gotextdiff.TextEdit {
	res := make([]gotextdiff.TextEdit, len(events))
	for i, ev := range events {
		res[i] = gotextdiff.TextEdit{
			Span: span.New(
				span.URI(""),
				span.NewPoint(int(ev.Range.Start.Line)+1, int(ev.Range.Start.Character)+1, -1),
				span.NewPoint(int(ev.Range.End.Line)+1, int(ev.Range.End.Character)+1, -1),
			),
			NewText: ev.Text,
		}
	}
	return res
}

type ParseResult struct {
	Root ast.Node
	Err  error
}

func (p *ParseResult) StaticErr() staticError {
	if p == nil {
		return nil
	}
	se, _ := p.Err.(staticError)
	return se
}

// AST recovery. Unfortunately jsonnet makes significant use of semicolons and colons for valid ASTs.
// As a user is typing, the AST will often be invalid due to a missing semicolon or comma.
// For example: `local x = std` -- when the user hits '.' autocomplete wont work because the
// previous AST could not be parsed (`local x = std;` is valid, however).
// The code below will try to add a semicolon and a comma to the text, the character after
// where the user is typing.
// We need still need to set the original AST error so it will be reported.
func tryRecoverAST(uri uri.URI, contents string, lastEdit *gotextdiff.TextEdit) ast.Node {
	// Eat panics from textedit
	defer func() { _ = recover() }()
	insertion := span.NewPoint(lastEdit.Span.End().Line(), lastEdit.Span.End().Column()+len(lastEdit.NewText), -1)
	addSemicol := []gotextdiff.TextEdit{{NewText: ";", Span: span.New(span.URI(""), insertion, insertion)}}
	addComma := []gotextdiff.TextEdit{{NewText: ",", Span: span.New(span.URI(""), insertion, insertion)}}

	withSemicol := gotextdiff.ApplyEdits(contents, addSemicol)
	if recovered, _ := jsonnet.SnippetToAST(uri.Filename(), withSemicol); recovered != nil {
		return recovered
	}

	withComma := gotextdiff.ApplyEdits(contents, addComma)
	if recovered, _ := jsonnet.SnippetToAST(uri.Filename(), withComma); recovered != nil {
		return recovered
	}

	return nil
}

func parseJsonnetFn(uri uri.URI) overlay.ParseFunc {
	return func(contents string, lastEdit *gotextdiff.TextEdit) (result interface{}, success bool) {
		// defer func(t time.Time) { logf("parsed ast len=%d in %s", len(contents), time.Since(t)) }(time.Now())
		res := &ParseResult{}
		res.Root, res.Err = jsonnet.SnippetToAST(uri.Filename(), contents)

		if res.Root == nil && lastEdit != nil {
			res.Root = tryRecoverAST(uri, contents, lastEdit)
		}

		return res, res.Root != nil
	}
}

func (s *Server) processFileUpdateFn(ctx context.Context, uri uri.URI) overlay.UpdateFunc {
	cvm := s.getVM(uri)
	ec := &ErrCollector{URI: uri, Diags: []protocol.Diagnostic{}}
	return func(ur overlay.UpdateResult) {
		// defer func(t time.Time) { logf("parsed done diags in %s", time.Since(t)) }(time.Now())
		if ur.Current == nil {
			return
		}

		if pr, _ := ur.Current.Data.(*ParseResult); pr.StaticErr() != nil {
			// AST failed to parse, do not run lints
			ec.Collect(pr.StaticErr(), protocol.DiagnosticSeverityError)
		} else if ur.Parsed != nil && ur.Current.Version == ur.Parsed.Version {
			// AST did parse, run linter
			cvm.Use(func(vm *jsonnet.VM) {
				vm.ErrorFormatter = ec
				snippets := []linter.Snippet{{FileName: uri.Filename(), Code: ur.Parsed.Contents}}
				linter.LintSnippet(vm, io.Discard, snippets)
				vm.ErrorFormatter = ErrDiscard{}
			})
		}

		_ = s.notifier.PublishDiagnostics(ctx, &protocol.PublishDiagnosticsParams{
			URI:         uri,
			Version:     uint32(ur.Current.Version),
			Diagnostics: ec.Diags,
		})
	}
}

type valueResolver struct {
	rootURI uri.URI
	rootAST ast.Node
	// A map of filenames from node.Loc().Filename to the root AST node
	// This is used to find the root AST node of any node.
	stackCache map[ast.Node][]ast.Node
	roots      map[string]ast.Node
	getvm      func() *vmCache
	vm         *vmCache
}

var _ = (analysis.Resolver)(new(valueResolver))

func (s *Server) NewResolver(uri uri.URI) *valueResolver {
	root := s.getCurrentAST(uri)
	if root == nil {
		return nil
	}
	return &valueResolver{
		rootURI:    uri,
		rootAST:    root,
		roots:      map[string]ast.Node{root.Loc().FileName: root},
		stackCache: map[ast.Node][]ast.Node{},
		getvm:      func() *vmCache { return s.getVM(uri) },
	}
}

func (r *valueResolver) NodeAt(loc ast.Location) (node ast.Node, stack []ast.Node) {
	stack = analysis.StackAtLoc(r.rootAST, loc)
	if len(stack) == 0 {
		return nil, nil
	}
	node = stack[len(stack)-1]
	r.stackCache[node] = stack
	return node, stack
}

func (r *valueResolver) Vars(from ast.Node) analysis.VarMap {
	root := r.roots[from.Loc().FileName]
	if root == nil {
		panic(fmt.Errorf("invariant: resolving var from %T where no root was imported", from))
	}
	if stk := r.stackCache[from]; len(stk) > 0 {
		return analysis.StackVars(stk)
	}
	stk := analysis.StackAtNode(root, from)
	return analysis.StackVars(stk)
}

func (r *valueResolver) Import(path string) ast.Node {
	// The reason for this dance is to only grab a VM and importer
	// if we need to import something. This allows us to avoid thrashing the
	// vm cache when we don't actually need a full VM to perform analysis
	if r.vm == nil {
		if r.getvm == nil {
			return nil
		}
		r.vm = r.getvm()
	}
	root, _ := r.vm.ImportAST(path)
	if root != nil {
		r.roots[root.Loc().FileName] = root
	}
	return root
}

func (s *Server) getCurrentAST(uri uri.URI) ast.Node {
	parsed := s.overlay.Parsed(uri)
	if parsed == nil {
		return nil
	}
	res, _ := parsed.Data.(*ParseResult)
	if res == nil || res.Root == nil {
		return nil
	}
	return res.Root
}