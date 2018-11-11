package langserver

import (
	"context"
	"errors"
	"fmt"
	"github.com/saibing/bingo/langserver/internal/refs"
	"github.com/saibing/bingo/langserver/internal/util"
	"github.com/saibing/bingo/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"
	"go/ast"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/packages"
	"log"
)

func (h *LangHandler) handleDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]lsp.Location, error) {
	res, err := h.handleXDefinition(ctx, conn, req, params)
	if err != nil {
		return nil, err
	}
	locs := make([]lsp.Location, 0, len(res))
	for _, li := range res {
		locs = append(locs, li.Location)
	}
	return locs, nil
}

func (h *LangHandler) handleTypeDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]lsp.Location, error) {
	res, err := h.handleXDefinition(ctx, conn, req, params)
	if err != nil {
		return nil, err
	}
	locs := make([]lsp.Location, 0, len(res))
	for _, li := range res {
		// not everything we find a definition for also has a type definition
		if li.TypeLocation.URI != "" {
			locs = append(locs, li.TypeLocation)
		}
	}
	return locs, nil
}

var testOSToVFSPath func(osPath string) string

type foundNode struct {
	ident *ast.Ident      // the lookup in Uses[] or Defs[]
	typ   *types.TypeName // the object for a named type, if present
}

func (h *LangHandler) handleXDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]symbolLocationInformation, error) {
	if !util.IsURI(params.TextDocument.URI) {
		return nil, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInvalidParams,
			Message: fmt.Sprintf("%s not yet supported for out-of-workspace URI (%q)", req.Method, params.TextDocument.URI),
		}
	}

	pkg, pos, err := h.typeCheck(params)
	if err != nil {
		// Invalid nodes means we tried to click on something which is
		// not an ident (eg comment/string/etc). Return no locations.
		if _, ok := err.(*util.InvalidNodeError); ok {
			return []symbolLocationInformation{}, nil
		}
		return nil, err
	}

	pathNodes, err := util.GetPathNodes(pkg, pos, pos)
	if err != nil {
		return nil, err
	}

	firstNode := pathNodes[0]
	switch node := firstNode.(type) {
	case *ast.Ident:
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, node)
	case *ast.TypeSpec:
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, node.Name)
	default:
		return nil, util.NewInvalidNodeError(pkg, firstNode)
	}
}

func (h *LangHandler) lookupIdentDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, pkg *packages.Package, pathNodes []ast.Node, ident *ast.Ident) ([]symbolLocationInformation, error) {

	var nodes []foundNode
	obj, ok := pkg.TypesInfo.Uses[ident]
	if !ok {
		obj, ok = pkg.TypesInfo.Defs[ident]
	}
	if ok && obj != nil {
		if p := obj.Pos(); p.IsValid() {
			nodes = append(nodes, foundNode{
				ident: &ast.Ident{NamePos: p, Name: obj.Name()},
				typ:   util.TypeLookup(pkg.TypesInfo.TypeOf(ident)),
			})
		} else {
			// Builtins have an invalid Pos. Just don't emit a definition for
			// them, for now. It's not that valuable to jump to their def.
			//
			// TODO(sqs): find a way to actually emit builtin locations
			// (pointing to builtin/builtin.go).
			return []symbolLocationInformation{}, nil
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("definition not found")
	}
	findPackage := h.getFindPackageFunc()
	locs := make([]symbolLocationInformation, 0, len(nodes))
	for _, found := range nodes {
		// Determine location information for the ident.
		l := symbolLocationInformation{
			Location: goRangeToLSPLocation(pkg.Fset, found.ident.Pos(), found.ident.End()),
		}
		if found.typ != nil {
			// We don't get an end position, but we can assume it's comparable to
			// the length of the name, I hope.
			l.TypeLocation = goRangeToLSPLocation(pkg.Fset, found.typ.Pos(), token.Pos(int(found.typ.Pos())+len(found.typ.Name())))
		}

		// Determine metadata information for the ident.
		if def, err := refs.DefInfo(pkg.Types, pkg.TypesInfo, pathNodes, found.ident.Pos()); err == nil {
			rootPath := h.FilePath(h.init.Root())
			symDesc, err := defSymbolDescriptor(ctx, conn, pkg, h.packageCache, rootPath, *def, findPackage)
			if err != nil {
				// TODO: tracing
				log.Println("refs.DefInfo:", err)
			} else {
				l.Symbol = symDesc
			}
		} else {
			// TODO: tracing
			log.Println("refs.DefInfo:", err)
		}
		locs = append(locs, l)
	}
	return locs, nil
}
