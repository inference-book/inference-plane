package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// gen-names flags. Defaults match the layout of this repo and the
// adjacent book repo so 'iplane gen-names' from the inference_plane
// root regenerates both files without further configuration.
var (
	gnYAMLPath string
	gnGoOut    string
	gnTexOut   string
)

var genNamesCmd = &cobra.Command{
	Use:   "gen-names",
	Short: "Regenerate the OTel name vocabulary (Go constants + book LaTeX)",
	Long: `Reads metric-names.yaml and emits two artifacts:

  - internal/telemetry/names.go (this repo)         compile-time Go consts
  - book/src/styles/metric-names.tex (book repo)    LaTeX commands

Both files carry DO NOT EDIT headers; both regenerate together. CI's
make check-names runs this and fails fast if the YAML was edited
without a corresponding regeneration.

The tool exists because OTel metric/attribute/label names are
referenced in code, chapter prose, dashboards, alert rules, and
PromQL queries. Drift between any of those is silently corrosive --
code-gen from one source eliminates the failure mode by construction.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGenNames(gnYAMLPath, gnGoOut, gnTexOut)
	},
}

func init() {
	rootCmd.AddCommand(genNamesCmd)
	genNamesCmd.Flags().StringVar(&gnYAMLPath, "yaml", "metric-names.yaml",
		"input YAML schema")
	genNamesCmd.Flags().StringVar(&gnGoOut, "go-out", "internal/telemetry/names.go",
		"output path for the Go constants file")
	genNamesCmd.Flags().StringVar(&gnTexOut, "tex-out", "../book/src/styles/metric-names.tex",
		"output path for the LaTeX commands file")
}

// genEntry is one named value in the schema.
type genEntry struct {
	Key         string `yaml:"key"`
	Value       string `yaml:"value"`
	Description string `yaml:"description,omitempty"`
}

// genSchema is the top-level YAML structure.
type genSchema struct {
	Metrics              []genEntry `yaml:"metrics"`
	InferenceAttributes  []genEntry `yaml:"inference_attributes"`
	DeploymentAttributes []genEntry `yaml:"deployment_attributes"`
	Labels               []genEntry `yaml:"labels"`
}

func runGenNames(yamlPath, goOut, texOut string) error {
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	var schema genSchema
	if err := yaml.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("parse schema: %w", err)
	}

	// Stable order so generated files don't churn from YAML reorders.
	for _, group := range [][]genEntry{schema.Metrics, schema.InferenceAttributes, schema.DeploymentAttributes, schema.Labels} {
		sort.Slice(group, func(i, j int) bool { return group[i].Key < group[j].Key })
	}

	if err := writeGoConsts(goOut, schema); err != nil {
		return fmt.Errorf("write Go: %w", err)
	}
	if err := writeTexCommands(texOut, schema); err != nil {
		return fmt.Errorf("write tex: %w", err)
	}

	fmt.Fprintf(os.Stderr, "iplane gen-names: wrote %s and %s\n", goOut, texOut)
	return nil
}

func writeGoConsts(path string, s genSchema) error {
	tmpl := template.Must(template.New("go").Funcs(template.FuncMap{
		"exported": exportedCamel,
	}).Parse(genGoTemplate))
	return renderTemplate(path, tmpl, s)
}

func writeTexCommands(path string, s genSchema) error {
	tmpl := template.Must(template.New("tex").Funcs(template.FuncMap{
		"exported": exportedCamel,
		"camel":    lowerCamel,
		"texValue": texEscape,
	}).Parse(genTexTemplate))
	return renderTemplate(path, tmpl, s)
}

func renderTemplate(path string, tmpl *template.Template, s genSchema) error {
	var buf strings.Builder
	if err := tmpl.Execute(&buf, s); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(buf.String()), 0644)
}

// exportedCamel converts snake_case keys into ExportedCamelCase for Go
// constants ('request_duration' -> 'RequestDuration'). Special cases
// uppercase the small set of acronyms we use (id, gpu) so the output
// matches Go's idiomatic style.
func exportedCamel(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		switch p {
		case "id":
			b.WriteString("ID")
			continue
		case "gpu":
			b.WriteString("GPU")
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// lowerCamel converts snake_case to lowerCamelCase for tex command
// names ('request_duration' -> 'requestDuration').
func lowerCamel(s string) string {
	parts := strings.Split(s, "_")
	var b strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			b.WriteString(p)
			continue
		}
		switch p {
		case "id":
			b.WriteString("ID")
			continue
		case "gpu":
			b.WriteString("GPU")
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// texEscape escapes the LaTeX-special characters that appear in
// metric/attribute values. The value goes inside \newcommand{}{...},
// where the body is reprocessed at expansion time, so unescaped
// underscores trigger math-mode errors.
func texEscape(s string) string {
	r := strings.NewReplacer(
		`\`, `\textbackslash{}`,
		`_`, `\_`,
		`&`, `\&`,
		`%`, `\%`,
		`#`, `\#`,
		`$`, `\$`,
		`{`, `\{`,
		`}`, `\}`,
		`~`, `\textasciitilde{}`,
		`^`, `\textasciicircum{}`,
	)
	return r.Replace(s)
}

const genGoTemplate = `// Code generated by iplane gen-names; DO NOT EDIT.
// Source: metric-names.yaml
//
// Run ` + "`make gen-names`" + ` to regenerate after editing the schema.
// Run ` + "`make check-names`" + ` (CI does this) to fail-fast if the schema
// has been edited without regenerating.

package telemetry

// Metric instrument names emitted by the control plane.
{{range .Metrics}}
// Metric{{exported .Key}} -- {{.Description}}
const Metric{{exported .Key}} = {{printf "%q" .Value}}
{{end}}

// Inference span and resource attribute keys.
const (
{{- range .InferenceAttributes}}
	AttrInference{{exported .Key}} = {{printf "%q" .Value}}
{{- end}}
)

// Deployment-identity resource attribute keys. These describe which
// provider/gpu_type/billing_mode/instance_id this control plane is
// running on, attached to every span and metric as resource attributes.
const (
{{- range .DeploymentAttributes}}
	AttrDeployment{{exported .Key}} = {{printf "%q" .Value}}
{{- end}}
)

// Metric label keys (the dimensions on counter/histogram/gauge instruments).
const (
{{- range .Labels}}
	Label{{exported .Key}} = {{printf "%q" .Value}}
{{- end}}
)
`

const genTexTemplate = `%% Auto-generated by iplane gen-names in the inference_plane repo;
%% DO NOT EDIT BY HAND. Run ` + "`make gen-names`" + ` to regenerate.
%% Source: inference_plane/metric-names.yaml

%% Metric instrument names.
{{range .Metrics -}}
\newcommand{\metric{{exported .Key}}}{ {{- texValue .Value -}} }
{{end}}
%% Inference span/resource attribute keys.
{{range .InferenceAttributes -}}
\newcommand{\attrInference{{exported .Key}}}{ {{- texValue .Value -}} }
{{end}}
%% Deployment-identity resource attribute keys.
{{range .DeploymentAttributes -}}
\newcommand{\attrDeployment{{exported .Key}}}{ {{- texValue .Value -}} }
{{end}}
%% Metric label keys.
{{range .Labels -}}
\newcommand{\label{{exported .Key}}}{ {{- texValue .Value -}} }
{{end}}`
