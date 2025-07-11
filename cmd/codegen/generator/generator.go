package generator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"dagger.io/dagger"
	"github.com/dagger/dagger/cmd/codegen/introspection"
)

var ErrUnknownSDKLang = errors.New("unknown sdk language")

type SDKLang string

const (
	SDKLangGo         SDKLang = "go"
	SDKLangTypeScript SDKLang = "typescript"
)

type ModuleSourceDependencies struct {
	Kind   string
	Name   string `json:"moduleOriginalName"`
	Pin    string
	Source string `json:"asString"`
}

type Config struct {
	// Lang is the language supported by this codegen infra.
	Lang SDKLang

	// OutputDir is the path to place generated code.
	OutputDir string

	// ModuleName is the module name to generate code for.
	ModuleName string

	// ModuleSourcePath is the subpath in OutputDir where a the module source subpath is located.
	ModuleSourcePath string

	// ModuleParentPath is the path from the module source subpath to the context directory
	ModuleParentPath string

	// IntrospectionJSON is an optional pre-computed introspection json string.
	IntrospectionJSON string

	// Merge indicates whether to merge the module deps with the existing project (i.e. a go.mod in a *parent* directory).
	Merge bool

	// Whether we are initializing a new module.
	// Currently, this is only used in go codegen to enforce backwards-compatible behavior
	// where a pre-existing go.mod file is checked during dagger init for whether its module
	// name is the expected value.
	IsInit bool

	// ClientOnly indicates that the codegen should only generate the client code.
	ClientOnly bool

	// The list of all dependencies used by the module.
	// This is used by the client generator to automatically serves the
	// dependencies when connecting to the client.
	ModuleDependencies []ModuleSourceDependencies

	// Generate the client in bundle mode.
	Bundle bool

	// A dagger client connected to the engine running the codegen.
	// This may be nil if the codegen is run outside of a dagger context and should
	// only be set if introspectionJSON or moduleSourceID are set.
	Dag *dagger.Client
}

type Generator interface {
	// GenerateModule runs codegen in a context of a module and returns a map of
	// default filename to content for that file.
	GenerateModule(ctx context.Context, schema *introspection.Schema, schemaVersion string) (*GeneratedState, error)

	// GenerateClient runs codegen in a context of a standalone client and returns
	// a map of default filename to content for that file.
	GenerateClient(ctx context.Context, schema *introspection.Schema, schemaVersion string) (*GeneratedState, error)
}

type GeneratedState struct {
	// Overlay is the overlay filesystem that contains generated code to write
	// over the output directory.
	Overlay fs.FS

	// PostCommands are commands that need to be run after the codegen has
	// finished. This is used for example to run `go mod tidy` after generating
	// Go code.
	PostCommands []*exec.Cmd

	// NeedRegenerate indicates that the code needs to be generated again. This
	// can happen if the codegen spat out templates that depend on generated
	// types. In that case the codegen needs to be run again with both the
	// templates and the initially generated types available.
	NeedRegenerate bool
}

// SetSchemaParents sets all the parents for the fields.
func SetSchemaParents(schema *introspection.Schema) {
	for _, t := range schema.Types {
		for _, f := range t.Fields {
			f.ParentObject = t
		}
	}
}

// Introspect gets the Dagger Schema
func Introspect(ctx context.Context, dag *dagger.Client) (*introspection.Schema, string, error) {
	var introspectionResp introspection.Response
	err := dag.Do(ctx, &dagger.Request{
		Query:  introspection.Query,
		OpName: "IntrospectionQuery",
	}, &dagger.Response{
		Data: &introspectionResp,
	})
	if err != nil {
		return nil, "", fmt.Errorf("introspection query: %w", err)
	}

	return introspectionResp.Schema, introspectionResp.SchemaVersion, nil
}

func Overlay(ctx context.Context, logsW io.Writer, overlay fs.FS, outputDir string) (rerr error) {
	return fs.WalkDir(overlay, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if _, err := os.Stat(filepath.Join(outputDir, path)); err == nil {
				fmt.Fprintln(logsW, "creating directory", path, "[skipped]")
				return nil
			}
			fmt.Fprintln(logsW, "creating directory", path)
			return os.MkdirAll(filepath.Join(outputDir, path), 0o755)
		}

		var needsWrite bool

		newContent, err := fs.ReadFile(overlay, path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}

		outPath := filepath.Join(outputDir, path)
		oldContent, err := os.ReadFile(outPath)
		if err != nil {
			needsWrite = true
		} else {
			needsWrite = string(oldContent) != string(newContent)
		}

		if !needsWrite {
			fmt.Fprintln(logsW, "writing", path, "[skipped]")
			return nil
		}

		fmt.Fprintln(logsW, "writing", path)
		return os.WriteFile(outPath, newContent, 0o600)
	})
}
