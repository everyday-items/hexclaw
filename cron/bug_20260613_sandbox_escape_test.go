package cron

// BUG-20260613 (security test closure on the Go sandbox): an attacker-shaped
// probe of the compile-time AST validator found six escape vectors its
// blocklist missed — os.popen/execv/spawnv (spawn a shell), importlib
// (re-import a banned module), getattr(os,"system") (defeat the call-name
// ban), and __builtins__["eval"] (subscript access). Blocklisting leaks by
// construction; the validator now enforces a stdlib module allowlist plus
// structural bans on the escape primitives. These cases lock the holes shut.

import (
	"strings"
	"testing"
)

func TestBug20260613_SandboxRejectsEscapeVectors(t *testing.T) {
	mustBlock := map[string]string{
		"os.popen":           `import os; os.popen("id").read()`,
		"os.system":          `import os; os.system("id")`,
		"subprocess":         `import subprocess; subprocess.run(["id"])`,
		"importlib":          `import importlib; importlib.import_module("subprocess").run(["id"])`,
		"getattr-os-system":  `import os; getattr(os, "system")("id")`,
		"builtins-subscript": `print(__builtins__["eval"]("1+1"))`,
		"os.execv":           `import os; os.execv("/bin/sh", ["sh"])`,
		"os.spawnv":          `import os; os.spawnv(os.P_WAIT, "/bin/sh", ["sh"])`,
		"pty":                `import pty; pty.spawn("/bin/sh")`,
		"ctypes":             `import ctypes; ctypes.CDLL("libc.so.6")`,
		"socket":             `import socket; socket.socket()`,
		"pickle":             `import pickle; pickle.loads(b"")`,
		"eval":               `eval("1+1")`,
		"exec":               `exec("x=1")`,
		"compile-builtin":    `compile("1", "x", "eval")`,
		"from-os-import":     `from os import system; system("id")`,
		"getattribute":       `import os; os.__getattribute__("system")("id")`,
		"import-star":        "from os import *\nsystem(\"id\")",
		"subclasses-gadget":  `().__class__.__bases__[0].__subclasses__()`,
		"mro-gadget":         `().__class__.__mro__[-1].__subclasses__()`,
		"lambda-globals":     `(lambda: 0).__globals__`,
		"vars-reflection":    `vars()`,
		"globals-reflection": `globals()`,
		"builtin-open-write": `open("/tmp/evil", "w").write("x")`,
		"os-raw-read":        `import os; fd = os.open("/etc/passwd", 0); os.read(fd, 100)`,
		"io-open":            `import io; io.open("/tmp/x", "w")`,
		"codecs-open":        `import codecs; codecs.open("/tmp/x", "w")`,
	}
	for name, src := range mustBlock {
		if err := validateNoForbiddenImports(src); err == nil {
			t.Errorf("[ESCAPE] %s must be rejected by the sandbox validator, but it passed: %s", name, src)
		}
	}
}

func TestBug20260613_SandboxAllowsLegitimateStdlib(t *testing.T) {
	mustPass := map[string]string{
		"urllib+re+json": `import urllib.request, re, json
html = urllib.request.urlopen("http://x").read().decode()
items = re.compile(r"<b>(.*?)</b>").findall(html)
print(json.dumps({"status": "success", "data": items}))`,
		"re.compile":  `import re; re.compile("x")`,
		"os.environ":  `import os, json; print(json.dumps({"status": "success", "data": os.environ.get("HEXCLAW_INPUTS", "")}))`,
		"datetime":    `import datetime, json; print(json.dumps({"status": "success", "data": str(datetime.date.today())}))`,
		"html.parser": `import html, json; print(json.dumps({"status": "success", "data": html.escape("<a>")}))`,
		"hashlib":     `import hashlib, json; print(json.dumps({"status": "success", "data": hashlib.sha256(b"x").hexdigest()}))`,
		"collections": `import collections, json; print(json.dumps({"status": "success", "data": list(collections.Counter("aab"))}))`,
	}
	for name, src := range mustPass {
		if err := validateNoForbiddenImports(src); err != nil {
			t.Errorf("[FALSE-POSITIVE] legitimate stdlib %s must pass, got: %v", name, err)
		}
	}
}

// Fuzz the validator: no input may panic or hang it, and known escape
// primitives must never pass regardless of surrounding code (method 11/1).
func FuzzSandboxValidator(f *testing.F) {
	f.Add(`import os; os.system("id")`)
	f.Add(`import urllib.request, json; print(json.dumps({}))`)
	f.Add(`from importlib import import_module`)
	f.Add(`getattr(__builtins__, "eval")`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, src string) {
		// Must terminate and not panic for any input.
		err := validateNoForbiddenImports(src)
		_ = err
		// If it parses and contains a hard escape token as a real call, it must
		// be blocked — spot-check the two most dangerous.
		for _, bad := range []string{`os.system("`, `subprocess.run(`} {
			if strings.Contains(src, bad) && err == nil {
				t.Errorf("escape token %q passed validation in: %q", bad, src)
			}
		}
	})
}
