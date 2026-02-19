package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/dotsetgreg/dotagent/pkg/agent"
	"github.com/dotsetgreg/dotagent/pkg/bus"
	"github.com/dotsetgreg/dotagent/pkg/config"
	"github.com/dotsetgreg/dotagent/pkg/cron"
	"github.com/dotsetgreg/dotagent/pkg/providers"
	"github.com/dotsetgreg/dotagent/pkg/tools"
	"github.com/spf13/cobra"
	cobraDoc "github.com/spf13/cobra/doc"
)

func newDocsCommand(rootFactory func() *cobra.Command) *cobra.Command {
	docsRoot := &cobra.Command{
		Use:    "docs",
		Short:  "Internal docs maintenance commands",
		Hidden: true,
	}

	var (
		outputDir string
		checkOnly bool
	)

	gen := &cobra.Command{
		Use:   "generate",
		Short: "Generate reference docs from command/config/provider/tool source",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(outputDir) == "" {
				return fmt.Errorf("--output must not be empty")
			}
			return generateDocumentation(rootFactory, outputDir, checkOnly)
		},
	}
	gen.Flags().StringVar(&outputDir, "output", "docs", "Docs directory root")
	gen.Flags().BoolVar(&checkOnly, "check", false, "Fail if generated docs are out of date")

	docsRoot.AddCommand(gen)
	return docsRoot
}

func generateDocumentation(rootFactory func() *cobra.Command, outputDir string, checkOnly bool) error {
	tmpDir, err := os.MkdirTemp("", "dotagent-docs-gen-*")
	if err != nil {
		return fmt.Errorf("create temp docs dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	generatedRoots, err := writeGeneratedReferences(rootFactory, tmpDir)
	if err != nil {
		return err
	}

	if checkOnly {
		for _, rel := range generatedRoots {
			if err := comparePath(filepath.Join(tmpDir, rel), filepath.Join(outputDir, rel), rel); err != nil {
				return err
			}
		}
		return nil
	}

	for _, rel := range generatedRoots {
		src := filepath.Join(tmpDir, rel)
		dst := filepath.Join(outputDir, rel)
		if err := copyPath(src, dst); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

func writeGeneratedReferences(rootFactory func() *cobra.Command, outDir string) ([]string, error) {
	cliRoot := rootFactory()
	markCommandsForDocgen(cliRoot)

	cliDir := filepath.Join(outDir, "reference", "cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cli docs dir: %w", err)
	}
	prepender := func(filename string) string {
		title := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
		title = strings.ReplaceAll(title, "_", " ")
		return fmt.Sprintf("# %s\n\n", strings.TrimSpace(title))
	}
	linkHandler := func(name string) string {
		return name
	}
	if err := cobraDoc.GenMarkdownTreeCustom(cliRoot, cliDir, prepender, linkHandler); err != nil {
		return nil, fmt.Errorf("generate cli markdown docs: %w", err)
	}

	manDir := filepath.Join(outDir, "reference", "man")
	if err := os.MkdirAll(manDir, 0o755); err != nil {
		return nil, fmt.Errorf("create man docs dir: %w", err)
	}
	header := &cobraDoc.GenManHeader{
		Title:   "DOTAGENT",
		Section: "1",
		Source:  "dotagent",
	}
	if err := cobraDoc.GenManTree(cliRoot, header, manDir); err != nil {
		return nil, fmt.Errorf("generate man pages: %w", err)
	}

	configRef, err := buildConfigReferenceMarkdown()
	if err != nil {
		return nil, err
	}
	if err := writeTextFile(filepath.Join(outDir, "reference", "config.md"), configRef); err != nil {
		return nil, err
	}

	providerRef, err := buildProvidersReferenceMarkdown()
	if err != nil {
		return nil, err
	}
	if err := writeTextFile(filepath.Join(outDir, "reference", "providers.md"), providerRef); err != nil {
		return nil, err
	}

	toolsRef, err := buildToolsReferenceMarkdown()
	if err != nil {
		return nil, err
	}
	if err := writeTextFile(filepath.Join(outDir, "reference", "tools.md"), toolsRef); err != nil {
		return nil, err
	}

	return []string{
		filepath.Join("reference", "cli"),
		filepath.Join("reference", "man"),
		filepath.Join("reference", "config.md"),
		filepath.Join("reference", "providers.md"),
		filepath.Join("reference", "tools.md"),
	}, nil
}

func markCommandsForDocgen(cmd *cobra.Command) {
	cmd.DisableAutoGenTag = true
	for _, child := range cmd.Commands() {
		if child.Name() == "docs" {
			continue
		}
		markCommandsForDocgen(child)
	}
}

func writeTextFile(path string, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", path, err)
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		_ = os.RemoveAll(dst)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			return copyFile(path, target)
		})
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func comparePath(src, dst, rel string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("generated path missing: %s (%w)", rel, err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		return fmt.Errorf("docs out of date: missing %s", rel)
	}

	if srcInfo.IsDir() != dstInfo.IsDir() {
		return fmt.Errorf("docs out of date: kind mismatch for %s", rel)
	}
	if !srcInfo.IsDir() {
		srcBytes, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		dstBytes, err := os.ReadFile(dst)
		if err != nil {
			return err
		}
		if !bytes.Equal(srcBytes, dstBytes) {
			return fmt.Errorf("docs out of date: %s differs; run `dotagent docs generate`", rel)
		}
		return nil
	}

	srcFiles, err := listFiles(src)
	if err != nil {
		return err
	}
	dstFiles, err := listFiles(dst)
	if err != nil {
		return err
	}
	if len(srcFiles) != len(dstFiles) {
		return fmt.Errorf("docs out of date: file count mismatch under %s", rel)
	}
	for i := range srcFiles {
		if srcFiles[i] != dstFiles[i] {
			return fmt.Errorf("docs out of date: file set mismatch under %s", rel)
		}
		srcPath := filepath.Join(src, srcFiles[i])
		dstPath := filepath.Join(dst, dstFiles[i])
		srcBytes, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		dstBytes, err := os.ReadFile(dstPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(srcBytes, dstBytes) {
			return fmt.Errorf("docs out of date: %s changed; run `dotagent docs generate`", filepath.Join(rel, srcFiles[i]))
		}
	}
	return nil
}

func listFiles(root string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

type configFieldRow struct {
	Path    string
	Type    string
	Env     string
	Default string
}

func buildConfigReferenceMarkdown() (string, error) {
	defaults, err := flattenConfigDefaults()
	if err != nil {
		return "", err
	}

	rows := []configFieldRow{}
	collectConfigRows(reflect.TypeOf(config.Config{}), "", defaults, &rows)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })

	var b strings.Builder
	b.WriteString("# Config Reference\n\n")
	b.WriteString("Generated from `pkg/config/config.go` and `config.DefaultConfig()`.\n\n")
	b.WriteString("| Key | Type | Env Var | Default |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, row := range rows {
		b.WriteString("| `" + escapePipes(row.Path) + "` | `" + escapePipes(row.Type) + "` | `" + escapePipes(valueOr(row.Env, "-")) + "` | `" + escapePipes(valueOr(row.Default, "-")) + "` |\n")
	}
	return b.String(), nil
}

func collectConfigRows(t reflect.Type, prefix string, defaults map[string]string, rows *[]configFieldRow) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := strings.TrimSpace(strings.Split(f.Tag.Get("json"), ",")[0])
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		path := jsonTag
		if prefix != "" {
			path = prefix + "." + jsonTag
		}

		fieldType := f.Type
		if fieldType.Kind() == reflect.Struct {
			collectConfigRows(fieldType, path, defaults, rows)
			continue
		}

		*rows = append(*rows, configFieldRow{
			Path:    path,
			Type:    friendlyType(fieldType),
			Env:     strings.TrimSpace(f.Tag.Get("env")),
			Default: defaults[path],
		})
	}
}

func flattenConfigDefaults() (map[string]string, error) {
	data, err := json.Marshal(config.DefaultConfig())
	if err != nil {
		return nil, err
	}
	var root map[string]interface{}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flattenMapValues("", root, out)
	return out, nil
}

func flattenMapValues(prefix string, v interface{}, out map[string]string) {
	switch typed := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			next := k
			if prefix != "" {
				next = prefix + "." + k
			}
			flattenMapValues(next, typed[k], out)
		}
	case []interface{}:
		encoded, _ := json.Marshal(typed)
		out[prefix] = string(encoded)
	default:
		encoded, _ := json.Marshal(typed)
		out[prefix] = string(encoded)
	}
}

func friendlyType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice:
		return "array<" + friendlyType(t.Elem()) + ">"
	case reflect.Map:
		return "map<" + friendlyType(t.Key()) + "," + friendlyType(t.Elem()) + ">"
	case reflect.Struct:
		return "object"
	case reflect.Pointer:
		return "*" + friendlyType(t.Elem())
	default:
		return t.String()
	}
}

type providerReferenceSpec struct {
	Name        string
	ConfigKey   string
	Summary     string
	AuthModel   string
	DefaultBase string
	StructType  reflect.Type
}

func buildProvidersReferenceMarkdown() (string, error) {
	defaults, err := flattenConfigDefaults()
	if err != nil {
		return "", err
	}

	cfgType := reflect.TypeOf(config.ProvidersConfig{})
	providerStructs := map[string]reflect.Type{}
	for i := 0; i < cfgType.NumField(); i++ {
		f := cfgType.Field(i)
		key := strings.TrimSpace(strings.Split(f.Tag.Get("json"), ",")[0])
		if key == "" || key == "-" {
			continue
		}
		providerStructs[key] = f.Type
	}

	specs := []providerReferenceSpec{
		{
			Name:        "openrouter",
			ConfigKey:   "providers.openrouter",
			Summary:     "OpenRouter chat completions provider.",
			AuthModel:   "Requires `api_key`.",
			DefaultBase: defaults["providers.openrouter.api_base"],
			StructType:  providerStructs["openrouter"],
		},
		{
			Name:        "openai",
			ConfigKey:   "providers.openai",
			Summary:     "OpenAI Platform provider (Responses API).",
			AuthModel:   "Requires exactly one credential source: `api_key` OR `oauth_access_token` OR `oauth_token_file`.",
			DefaultBase: defaults["providers.openai.api_base"],
			StructType:  providerStructs["openai"],
		},
		{
			Name:        "openai-codex",
			ConfigKey:   "providers.openai_codex",
			Summary:     "ChatGPT/Codex OAuth provider for Codex backend routing.",
			AuthModel:   "Requires exactly one credential source: `oauth_access_token` OR `oauth_token_file`.",
			DefaultBase: defaults["providers.openai_codex.api_base"],
			StructType:  providerStructs["openai_codex"],
		},
	}

	supported := providers.SupportedProviders()
	sort.Strings(supported)

	var b strings.Builder
	b.WriteString("# Provider Reference\n\n")
	b.WriteString("Generated from provider factories and config structs.\n\n")
	b.WriteString("## Supported Providers\n\n")
	for _, name := range supported {
		b.WriteString("- `" + name + "`\n")
	}
	b.WriteString("\n")

	for _, spec := range specs {
		b.WriteString("## `" + spec.Name + "`\n\n")
		b.WriteString(spec.Summary + "\n\n")
		b.WriteString("- Config path: `" + spec.ConfigKey + "`\n")
		b.WriteString("- Auth: " + spec.AuthModel + "\n")
		if strings.TrimSpace(spec.DefaultBase) != "" {
			b.WriteString("- Default API base: `" + spec.DefaultBase + "`\n")
		}
		b.WriteString("\n")

		rows := []configFieldRow{}
		collectProviderFields(spec.StructType, spec.ConfigKey, defaults, &rows)
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })

		b.WriteString("| Key | Type | Env Var | Default |\n")
		b.WriteString("| --- | --- | --- | --- |\n")
		for _, row := range rows {
			b.WriteString("| `" + escapePipes(row.Path) + "` | `" + escapePipes(row.Type) + "` | `" + escapePipes(valueOr(row.Env, "-")) + "` | `" + escapePipes(valueOr(row.Default, "-")) + "` |\n")
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}

func collectProviderFields(t reflect.Type, prefix string, defaults map[string]string, rows *[]configFieldRow) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := strings.TrimSpace(strings.Split(f.Tag.Get("json"), ",")[0])
		if jsonTag == "" || jsonTag == "-" {
			continue
		}
		path := prefix + "." + jsonTag
		*rows = append(*rows, configFieldRow{
			Path:    path,
			Type:    friendlyType(f.Type),
			Env:     strings.TrimSpace(f.Tag.Get("env")),
			Default: defaults[path],
		})
	}
}

type docsStubProvider struct{}

func (docsStubProvider) Chat(ctx context.Context, messages []providers.Message, tools []providers.ToolDefinition, model string, options map[string]interface{}) (*providers.LLMResponse, error) {
	return nil, fmt.Errorf("docs stub provider: chat is not supported")
}

func (docsStubProvider) GetDefaultModel() string {
	return "docs-stub"
}

func buildToolsReferenceMarkdown() (string, error) {
	tmpWorkspace, err := os.MkdirTemp("", "dotagent-docs-tools-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpWorkspace)

	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = tmpWorkspace
	cfg.Agents.Defaults.Model = "docs-stub"

	msgBus := bus.NewMessageBus()
	loop, err := agent.NewAgentLoop(cfg, msgBus, docsStubProvider{})
	if err != nil {
		return "", fmt.Errorf("build tool reference: %w", err)
	}
	defer loop.Stop()

	startup := loop.GetStartupInfo()
	toolBlock, ok := startup["tools"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("tool startup block missing")
	}

	summariesRaw, ok := toolBlock["summaries"].([]string)
	if !ok {
		return "", fmt.Errorf("tool summaries missing from startup info")
	}

	toolRows := map[string]string{}
	for _, line := range summariesRaw {
		name, desc := parseToolSummary(line)
		if name == "" {
			continue
		}
		toolRows[name] = desc
	}

	cronStore := filepath.Join(tmpWorkspace, "cron", "jobs.json")
	cronService := cron.NewCronService(cronStore, nil)
	cronTool := tools.NewCronTool(cronService, loop, msgBus, tmpWorkspace, cfg.Agents.Defaults.RestrictToWorkspace)
	toolRows[cronTool.Name()] = cronTool.Description()

	names := make([]string, 0, len(toolRows))
	for name := range toolRows {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# Tool Reference\n\n")
	b.WriteString("Generated from runtime tool registration and tool descriptions.\n\n")
	b.WriteString("## Built-In Tools\n\n")
	b.WriteString("| Tool | Description |\n")
	b.WriteString("| --- | --- |\n")
	for _, name := range names {
		b.WriteString("| `" + escapePipes(name) + "` | " + escapePipes(toolRows[name]) + " |\n")
	}

	b.WriteString("\n## Tool Policy Modes\n\n")
	b.WriteString("| Mode | Behavior |\n")
	b.WriteString("| --- | --- |\n")
	b.WriteString("| `auto` | Chooses `workspace_ops` for workspace/system intent, otherwise `conversation`. |\n")
	b.WriteString("| `conversation` | Restricts to conversational-safe tools (default web tools unless overridden by allow/deny). |\n")
	b.WriteString("| `workspace_ops` | Enables local workspace/system tooling (still subject to deny rules and protected state-path checks). |\n")

	b.WriteString("\n## Notes\n\n")
	b.WriteString("- `agents.defaults.restrict_to_workspace` controls shell/path guard strictness inside command tools.\n")
	b.WriteString("- `tools.policy.*` controls per-turn tool exposure and per-provider defaults.\n")

	return b.String(), nil
}

func parseToolSummary(line string) (string, string) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "- `") {
		return "", ""
	}
	trimmed = strings.TrimPrefix(trimmed, "- `")
	parts := strings.SplitN(trimmed, "` - ", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}

func escapePipes(v string) string {
	return strings.ReplaceAll(v, "|", "\\|")
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
