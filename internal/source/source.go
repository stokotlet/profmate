package source

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stokotlet/profmate/internal/model"
)

const maxContext = 32 << 10

func Extract(root string, a model.Analysis) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("нужна папка с исходниками")
	}
	rootFS, err := os.OpenRoot(abs)
	if err != nil {
		return "", err
	}
	defer rootFS.Close()
	var files []string
	count := 0
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		count++
		if count > 20000 {
			return errors.New("слишком большая папка: укажи корень одного Go-проекта")
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() && path != abs && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "node_modules" || d.Name() == "profmate-runs") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	var targets []model.Frame
	seen := map[string]bool{}
	for _, m := range a.Metrics {
		for _, stack := range m.Stacks {
			for i := len(stack.Frames) - 1; i >= 0; i-- {
				f := stack.Frames[i]
				key := fmt.Sprintf("%s:%d", f.File, f.Line)
				if f.File != "" && f.Line > 0 && !seen[key] {
					targets = append(targets, f)
					seen[key] = true
				}
			}
		}
		for _, f := range m.Top {
			key := fmt.Sprintf("%s:%d", f.File, f.Line)
			if f.File != "" && f.Line > 0 && !seen[key] {
				targets = append(targets, f.Frame)
				seen[key] = true
			}
		}
	}
	var b strings.Builder
	snippets := map[string]bool{}
	for _, target := range targets {
		path := resolve(abs, files, target.File)
		if path == "" {
			continue
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			continue
		}
		data, err := readSource(rootFS, rel)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		line := int(target.Line)
		if line < 1 || line > len(lines) {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, data, 0)
		if err != nil {
			continue
		}
		start, end := 0, 0
		for _, item := range file.Decls {
			decl, ok := item.(*ast.FuncDecl)
			if !ok || !matchesFunction(target.Name, file.Name.Name, decl) {
				continue
			}
			lo, hi := fset.Position(decl.Pos()).Line, fset.Position(decl.End()).Line
			if line < lo || line > hi {
				continue
			}
			start, end = lo, hi
			if end-start+1 > 80 {
				start = max(lo, line-59) // keep the sampled line inside the window
				end = min(hi, start+79)
				start = max(lo, end-79)
			}
			break
		}
		if start == 0 {
			continue
		}
		key := fmt.Sprintf("%s:%d", path, start)
		if snippets[key] {
			continue
		}
		snippets[key] = true
		var snippet strings.Builder
		fmt.Fprintf(&snippet, "\nSOURCE %s:%d-%d (profile function %s)\n", filepath.ToSlash(rel), start, end, target.Name)
		for i := start; i <= end; i++ {
			fmt.Fprintf(&snippet, "%4d | %s\n", i, lines[i-1])
		}
		if b.Len()+snippet.Len() > maxContext {
			break
		}
		b.WriteString(snippet.String())
		if len(snippets) >= 8 {
			break
		}
	}
	if b.Len() == 0 {
		return "", errors.New("не нашёл совпадающих файлов и строк из профиля; проверь путь и версию исходников")
	}
	return "Исходники предоставлены пользователем. Соответствие сборке автоматически не проверено.\n" + b.String(), nil
}

func readSource(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() > 1<<20 {
		return nil, errors.New("исходник должен быть обычным файлом не больше 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("исходник больше 1 MiB")
	}
	return data, nil
}

func matchesFunction(symbol, packageName string, decl *ast.FuncDecl) bool {
	symbol = withoutTypeArguments(strings.TrimSuffix(symbol, "-fm"))
	for {
		prefix, suffix, found := cutLast(symbol, ".")
		if !found || !generatedSuffix(suffix) {
			break
		}
		symbol = prefix
	}
	name := "." + decl.Name.Name
	var candidates []string
	if decl.Recv == nil {
		candidates = []string{name}
	} else if len(decl.Recv.List) == 1 {
		receiver := receiverName(decl.Recv.List[0].Type)
		if receiver != "" {
			candidates = []string{"." + receiver + name, ".(*" + receiver + ")" + name}
		}
	}
	for _, candidate := range candidates {
		if !strings.HasSuffix(symbol, candidate) {
			continue
		}
		prefix := strings.TrimSuffix(symbol, candidate)
		parts := strings.Split(prefix, "/")
		pkg := parts[len(parts)-1]
		if pkg == packageName || strings.HasPrefix(pkg, packageName+".v") && digits(strings.TrimPrefix(pkg, packageName+".v")) {
			return true
		}
		if len(parts) > 1 && strings.HasPrefix(pkg, "v") && digits(pkg[1:]) && parts[len(parts)-2] == packageName {
			return true
		}
	}
	return false
}

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	}
	return ""
}

func withoutTypeArguments(symbol string) string {
	var b strings.Builder
	depth := 0
	for _, r := range symbol {
		if r == '[' {
			depth++
		} else if r == ']' && depth > 0 {
			depth--
		} else if depth == 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func cutLast(s, separator string) (string, string, bool) {
	i := strings.LastIndex(s, separator)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(separator):], true
}

func generatedSuffix(s string) bool {
	if digits(s) {
		return true
	}
	for _, prefix := range []string{"func", "gowrap", "deferwrap"} {
		if strings.HasPrefix(s, prefix) && digits(strings.TrimPrefix(s, prefix)) {
			return true
		}
	}
	return false
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func resolve(root string, files []string, profilePath string) string {
	clean := filepath.Clean(filepath.FromSlash(profilePath))
	if filepath.IsAbs(clean) {
		if rel, err := filepath.Rel(root, clean); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			for _, f := range files {
				if f == clean {
					return f
				}
			}
		}
	}
	best := ""
	score := 0
	ambiguous := false
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for _, f := range files {
		other := strings.Split(filepath.ToSlash(f), "/")
		n := 0
		for i, j := len(parts)-1, len(other)-1; i >= 0 && j >= 0 && parts[i] == other[j]; i, j = i-1, j-1 {
			n++
		}
		if n > score {
			best = f
			score = n
			ambiguous = false
		} else if n == score && n > 0 {
			ambiguous = true
		}
	}
	if score == 0 || ambiguous {
		return ""
	}
	return best
}
