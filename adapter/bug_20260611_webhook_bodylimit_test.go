package adapter

// BUG-20260611 (review High): every IM platform webhook handler read the
// request body with io.ReadAll(r.Body) and no size limit — an external caller
// could stream an arbitrarily large payload and OOM the sidecar.
//
// This test discovers every adapter source file dynamically (no hardcoded
// list): it walks adapter/ for functions that call io.ReadAll(r.Body) and
// requires an http.MaxBytesReader guard applied EARLIER IN THE SAME FUNCTION.
// A new adapter that adds an unguarded ReadAll fails here automatically; the
// granularity matches the bug (per handler function, not per file).

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBug20260611_AllWebhookHandlersLimitBody(t *testing.T) {
	const readCall = "io.ReadAll(r.Body)"
	const guardCall = "http.MaxBytesReader("

	handlersFound := 0
	walkErr := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(data, []byte(readCall)) {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, data, 0)
		if perr != nil {
			t.Errorf("parse %s: %v", path, perr)
			return nil
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			start := fset.Position(fn.Body.Pos()).Offset
			end := fset.Position(fn.Body.End()).Offset
			body := string(data[start:end])

			readIdx := strings.Index(body, readCall)
			if readIdx < 0 {
				continue
			}
			handlersFound++
			guardIdx := strings.Index(body, guardCall)
			if guardIdx < 0 || guardIdx > readIdx {
				t.Errorf("[BUG-20260611] %s: func %s reads r.Body with %s but has no preceding %s guard — OOM surface",
					path, fn.Name.Name, readCall, guardCall)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk adapter/: %v", walkErr)
	}
	// Five adapters (dingtalk/feishu/slack/wechat/wecom) are known to read the
	// webhook body today; finding fewer means the scan itself broke.
	if handlersFound < 5 {
		t.Fatalf("source scan found only %d handler(s) calling %s — expected at least 5; scan may be broken", handlersFound, readCall)
	}
}
