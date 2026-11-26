package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The TypeScript and Python adapters are separate processes like the Java one. Their sources, and the TypeScript
// compiler they use, are embedded and unpacked into the user cache on first use, so nothing from the analyzed
// repository (in particular nothing under its node_modules) is ever executed.

//go:embed adapter/ts-adapter.js
var tsAdapterSrc []byte

//go:embed adapter/typescript-5.9.3.tar.gz
var tsCompilerArchive []byte

//go:embed adapter/py_adapter.py
var pyAdapterSrc []byte

// family groups languages that one adapter handles together.
func family(lang string) string {
	switch lang {
	case "typescript", "javascript", "vue":
		return "ts"
	case "python":
		return "py"
	}
	return lang
}

func cacheDir(name string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "rintel", name), nil
}

// tsAdapterDir unpacks the adapter and the bundled compiler once per version.
func tsAdapterDir() (string, error) {
	dir, err := cacheDir("ts-" + hashOf(append(append([]byte{}, tsAdapterSrc...), tsCompilerArchive...))[:12])
	if err != nil {
		return "", err
	}
	if fileExists(filepath.Join(dir, ".ready")) {
		return dir, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(tsCompilerArchive))
	if err != nil {
		return "", err
	}
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		target := filepath.Join(dir, filepath.FromSlash(h.Name))
		if rel, err := filepath.Rel(dir, target); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("embedded archive entry escapes its directory: %s", h.Name)
		}
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ts-adapter.js"), tsAdapterSrc, 0o644); err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, ".ready"), nil, 0o644)
}

func pyAdapterFile() (string, error) {
	dir, err := cacheDir("py-" + hashOf(pyAdapterSrc)[:12])
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, "py_adapter.py")
	if fileExists(file) {
		return file, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return file, os.WriteFile(file, pyAdapterSrc, 0o644)
}

// runProcess feeds one JSON request to an adapter process and parses its JSON Lines output.
func runProcess(name string, req any, bin string, args ...string) ([]irLine, error) {
	in, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := stderr.String()
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return nil, fmt.Errorf("%s adapter: %v\n%s", name, err, msg)
	}
	return parseIR(name, out)
}

func parseIR(name string, out []byte) ([]irLine, error) {
	var lines []irLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l irLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s adapter emitted invalid IR: %v", name, err)
		}
		if l.Kind != "header" {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}

func tsAdapter(root, module string, files, emit []string) ([]irLine, error) {
	dir, err := tsAdapterDir()
	if err != nil {
		return nil, err
	}
	return runProcess("TypeScript", map[string]any{"protocol": 1, "root": root, "module": module, "files": files, "emit": emit},
		"node", "--max-old-space-size=4096", filepath.Join(dir, "ts-adapter.js"))
}

func pyAdapter(root string, files, emit []string) ([]irLine, error) {
	file, err := pyAdapterFile()
	if err != nil {
		return nil, err
	}
	// -I: isolated mode, so the repository cannot inject modules through the working directory or environment
	return runProcess("Python", map[string]any{"protocol": 1, "root": root, "files": files, "emit": emit}, "python3", "-I", file)
}

// toolchains reports which adapters can run here. Analysis of one language never depends on another's toolchain.
func toolchains() map[string]bool {
	have := func(bin string) bool { _, err := exec.LookPath(bin); return err == nil }
	return map[string]bool{"java": have("java") && have("javac"), "go": have("go"), "ts": have("node"), "py": have("python3")}
}

var toolchainHelp = map[string]string{
	"java": "Java files were not analyzed: no JDK with javac on PATH",
	"go":   "Go files were not analyzed: the Go toolchain is not on PATH",
	"ts":   "TypeScript/JavaScript files were not analyzed: node is not on PATH",
	"py":   "Python files were not analyzed: python3 is not on PATH",
}
