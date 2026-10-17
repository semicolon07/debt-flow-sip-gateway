package deployment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// The Gateway deliberately has no control-plane route or credential for Debt Flow Web API.
// Browser-relayed capabilities are the only integration boundary, so fail CI if production
// code starts constructing a generic HTTP client or importing a common outbound HTTP SDK.
func TestProductionCodeDoesNotIntroduceAnOutboundHTTPClient(t *testing.T) {
	root := moduleRoot(t)
	for _, directory := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checkNoOutboundHTTPClient(t, root, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walk production Go sources: %v", err)
		}
	}
}

func checkNoOutboundHTTPClient(t *testing.T, root, path string) {
	t.Helper()
	set := token.NewFileSet()
	file, err := parser.ParseFile(set, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	httpAliases := make(map[string]struct{})
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			t.Fatalf("parse import in %s: %v", path, err)
		}
		switch importPath {
		case "github.com/go-resty/resty/v2", "github.com/hashicorp/go-retryablehttp", "net/http/cookiejar":
			t.Errorf("%s imports outbound HTTP package %q", relativePath(root, path), importPath)
		case "net/http":
			alias := "http"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			if alias == "." {
				t.Errorf("%s dot-imports net/http, which defeats the no-client architecture check", relativePath(root, path))
				continue
			}
			if alias != "_" {
				httpAliases[alias] = struct{}{}
			}
		}
	}
	forbidden := map[string]struct{}{
		"Client": {}, "DefaultClient": {}, "DefaultTransport": {}, "Get": {}, "Head": {},
		"NewRequest": {}, "NewRequestWithContext": {}, "Post": {}, "PostForm": {},
		"ProxyFromEnvironment": {}, "ProxyURL": {}, "Transport": {},
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		identifier, ok := selector.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isHTTP := httpAliases[identifier.Name]; !isHTTP {
			return true
		}
		if _, disallowed := forbidden[selector.Sel.Name]; disallowed {
			position := set.Position(selector.Pos())
			t.Errorf("%s:%d uses net/http.%s; Gateway production code must not create outbound HTTP clients",
				relativePath(root, path), position.Line, selector.Sel.Name)
		}
		return true
	})
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func relativePath(root, path string) string {
	value, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(value)
}
