package main

import (
	"bufio"
	"fmt"
	"go/build"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PackageFiles 는 디렉터리 하나의 Go 파일을 현재 빌드 컨텍스트 기준으로 분류한 것이다.
//
// go list 를 쓰지 않고 파일시스템을 직접 걷는 이유가 있다. go list ./... 는 현재 GOOS 에서
// 활성 파일이 하나도 없는 디렉터리를 아예 보고하지 않는다. 그러면 windows 전용 파일만 있는
// 패키지는 IgnoredGoFiles 를 읽을 기회조차 없이 게이트 밖이 된다.
type PackageFiles struct {
	Dir        string
	ImportPath string
	Name       string   // 패키지 이름. 활성 파일이 없으면 빈 문자열
	GoFiles    []string // 현재 컨텍스트에서 빌드되는 파일
	CgoFiles   []string // import "C" 를 포함하는 활성 파일
	IgnoredGo  []string // 빌드 태그나 파일명 접미사로 배제된 파일 (비테스트)
	TestGo     []string // 같은 패키지 이름의 _test.go
	XTestGo    []string // 외부 테스트 패키지의 _test.go
	Imports    []string // 활성 비테스트 파일의 직접 import
	TestImport []string // 테스트 파일의 직접 import
}

// Built 는 이 컨텍스트에서 컴파일되는 비테스트 파일 전부다.
func (p PackageFiles) Built() []string {
	return append(append([]string{}, p.GoFiles...), p.CgoFiles...)
}

// ReadModulePath 는 go.mod 의 module 줄을 읽는다.
func ReadModulePath(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	return "", fmt.Errorf("go.mod 에 module 줄이 없다")
}

// Discover 는 root 아래의 모든 Go 패키지 디렉터리를 찾아 분류한다.
// vendor, testdata, 숨김 디렉터리, 하위 모듈(별도 go.mod)은 건너뛴다.
func Discover(root, module string, ctx *build.Context) ([]PackageFiles, error) {
	var out []PackageFiles
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if p != absRoot {
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir // 하위 모듈은 자기 게이트를 갖는다
			}
		}
		hasGo := false
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				hasGo = true
				break
			}
		}
		if !hasGo {
			return nil
		}
		bp, err := ctx.ImportDir(p, 0)
		if err != nil {
			// 활성 파일이 없는 디렉터리도 IgnoredGoFiles 는 채워져서 돌아온다.
			// 그 파일들을 검사하기 위해 오류를 삼킨다. 다른 오류는 올린다.
			if _, ok := err.(*build.NoGoError); !ok {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		rel, err := filepath.Rel(absRoot, p)
		if err != nil {
			return err
		}
		importPath := module
		if rel != "." {
			importPath = module + "/" + filepath.ToSlash(rel)
		}
		pf := PackageFiles{
			Dir:        p,
			ImportPath: importPath,
			Name:       bp.Name,
			GoFiles:    bp.GoFiles,
			CgoFiles:   bp.CgoFiles,
			IgnoredGo:  bp.IgnoredGoFiles,
			TestGo:     bp.TestGoFiles,
			XTestGo:    bp.XTestGoFiles,
			Imports:    bp.Imports,
		}
		pf.TestImport = append(append([]string{}, bp.TestImports...), bp.XTestImports...)
		// go/build 는 배제된 테스트 파일을 IgnoredGoFiles 에 섞어 넣는다. 테스트로 분류한다.
		var ignored []string
		for _, f := range pf.IgnoredGo {
			if strings.HasSuffix(f, "_test.go") {
				pf.TestGo = append(pf.TestGo, f)
			} else {
				ignored = append(ignored, f)
			}
		}
		pf.IgnoredGo = ignored
		sort.Strings(pf.TestGo)
		out = append(out, pf)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImportPath < out[j].ImportPath })
	return out, nil
}
