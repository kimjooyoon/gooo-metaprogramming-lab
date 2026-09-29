package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const caseSchema = "gooo/metaprogramming-lab-case/v1"

type Case struct {
	Schema          string           `json:"schema"`
	ID              string           `json:"id"`
	Task            string           `json:"task"`
	Package         string           `json:"package"`
	BaseIR          IR               `json:"base_ir"`
	Transformations []Transformation `json:"transformations"`
	Fallback        string           `json:"fallback"`
	Required        Requirements     `json:"required"`
	Behavioral      string           `json:"behavioral_contract"`
}

type IR struct {
	Entities   []string   `json:"entities"`
	Activities []Activity `json:"activities"`
}

type Activity struct {
	Name    string   `json:"name"`
	Inputs  []string `json:"inputs"`
	Outputs []string `json:"outputs"`
}

type Transformation struct {
	ID            string     `json:"id"`
	Description   string     `json:"description"`
	AddEntities   []string   `json:"add_entities"`
	AddActivities []Activity `json:"add_activities"`
}

type Requirements struct {
	Entities   []string   `json:"entities"`
	Activities []Activity `json:"activities"`
}

type decisionOption struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type decisionQuestion struct {
	ID           string           `json:"id"`
	Instructions string           `json:"instructions"`
	Options      []decisionOption `json:"options"`
}

type decisionRequest struct {
	Schema   string           `json:"schema"`
	State    string           `json:"state"`
	Question decisionQuestion `json:"question"`
	Fallback string           `json:"fallback"`
}

type DecisionReceipt struct {
	Schema           string             `json:"schema"`
	Mode             string             `json:"mode"`
	Selected         string             `json:"selected"`
	FallbackReason   string             `json:"fallback_reason,omitempty"`
	Provider         string             `json:"provider"`
	Model            string             `json:"model,omitempty"`
	ModelRevision    string             `json:"model_revision,omitempty"`
	Routing          map[string]any     `json:"routing,omitempty"`
	Probabilities    map[string]float64 `json:"probabilities,omitempty"`
	Confidence       *float64           `json:"confidence,omitempty"`
	AnswerConfidence *float64           `json:"answer_confidence,omitempty"`
	RequestSHA256    string             `json:"request_sha256"`
}

type Coverage struct {
	Satisfied int      `json:"satisfied"`
	Total     int      `json:"total"`
	Ratio     float64  `json:"ratio"`
	Missing   []string `json:"missing,omitempty"`
}

type CandidateResult struct {
	ID              string   `json:"id"`
	IRValid         bool     `json:"ir_valid"`
	Compiled        bool     `json:"compiled"`
	Deterministic   bool     `json:"deterministic"`
	IRSHA256        string   `json:"ir_sha256,omitempty"`
	GeneratedSHA256 string   `json:"generated_sha256,omitempty"`
	ReplaySHA256    string   `json:"replay_sha256,omitempty"`
	ReplayCount     int      `json:"replay_count"`
	GeneratedBytes  int64    `json:"generated_bytes,omitempty"`
	GenerationMS    float64  `json:"generation_ms"`
	ReplayMS        float64  `json:"replay_ms"`
	CompileMS       float64  `json:"compile_ms"`
	Coverage        Coverage `json:"structural_completeness"`
	Error           string   `json:"error,omitempty"`
}

type Report struct {
	Schema            string            `json:"schema"`
	CaseID            string            `json:"case_id"`
	EvaluationScope   string            `json:"evaluation_scope"`
	Decision          DecisionReceipt   `json:"decision"`
	DecisionLatencyMS float64           `json:"decision_latency_ms"`
	BaselineID        string            `json:"baseline_id"`
	SelectedID        string            `json:"selected_id"`
	OracleID          string            `json:"oracle_id"`
	SelectedDelta     float64           `json:"selected_delta_vs_baseline"`
	OracleGap         float64           `json:"oracle_gap"`
	Requirements      Requirements      `json:"requirements"`
	Candidates        []CandidateResult `json:"candidates"`
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	packagePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	strategyPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "matrix" {
		if err := runMatrixCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "lab matrix: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: lab run --case <case.json> --out <directory> [--gooo <binary>] | lab matrix --plan <plan.json> --out <directory> [--gooo <binary>]")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	casePath := flags.String("case", "", "case JSON path")
	outDir := flags.String("out", "", "output directory for candidate artifacts and report")
	goooBin := flags.String("gooo", os.Getenv("GOOO_BIN"), "Gooo CLI path; defaults to GOOO_BIN or gooo")
	if err := flags.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	if *casePath == "" || *outDir == "" {
		fmt.Fprintln(os.Stderr, "lab: --case and --out are required")
		os.Exit(2)
	}
	if *goooBin == "" {
		*goooBin = "gooo"
	}
	report, err := run(*casePath, *outDir, *goooBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab: %v\n", err)
		os.Exit(1)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab: encode report: %v\n", err)
		os.Exit(1)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "lab: create output directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(*outDir, "report.json"), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "lab: write report: %v\n", err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(data)
}

func run(casePath, outDir, goooBin string) (Report, error) {
	data, err := os.ReadFile(casePath)
	if err != nil {
		return Report{}, fmt.Errorf("read case: %w", err)
	}
	var c Case
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return Report{}, fmt.Errorf("decode case: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Report{}, errors.New("case contains trailing JSON content")
	}
	if err := validateCase(c); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Report{}, fmt.Errorf("create output directory: %w", err)
	}
	request := makeDecisionRequest(c)
	started := time.Now()
	decision, err := decide(goooBin, request)
	decisionMS := float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		return Report{}, err
	}
	strategies := append([]Transformation(nil), c.Transformations...)
	results := make([]CandidateResult, 0, len(strategies))
	for _, strategy := range strategies {
		ir := apply(c.BaseIR, strategy)
		result := generateCandidate(goooBin, c, strategy.ID, ir, outDir)
		results = append(results, result)
	}
	selected, ok := resultByID(results, decision.Selected)
	if !ok {
		return Report{}, fmt.Errorf("decision selected unknown transformation %q", decision.Selected)
	}
	baseline, ok := resultByID(results, c.Fallback)
	if !ok {
		return Report{}, errors.New("fallback candidate result is missing")
	}
	oracle, ok := oracleCandidate(results)
	if !ok {
		return Report{}, errors.New("no candidate passed generated-Go compilation")
	}
	return Report{
		Schema: "gooo/metaprogramming-lab-report/v1", CaseID: c.ID,
		EvaluationScope: c.Behavioral, Decision: decision, DecisionLatencyMS: decisionMS,
		BaselineID: c.Fallback, SelectedID: selected.ID, OracleID: oracle.ID,
		SelectedDelta: selected.Coverage.Ratio - baseline.Coverage.Ratio,
		OracleGap:     oracle.Coverage.Ratio - selected.Coverage.Ratio,
		Requirements:  c.Required, Candidates: results,
	}, nil
}

func validateCase(c Case) error {
	if c.Schema != caseSchema || c.ID == "" || c.Task == "" || !packagePattern.MatchString(c.Package) {
		return errors.New("case has invalid schema, id, task, or package")
	}
	if len(c.Transformations) < 2 || len(c.Transformations) > 20 || c.Fallback == "" {
		return errors.New("case requires 2–20 transformations and an explicit fallback")
	}
	seen := make(map[string]bool, len(c.Transformations))
	fallbackFound := false
	for _, strategy := range c.Transformations {
		if !strategyPattern.MatchString(strategy.ID) || strategy.Description == "" || seen[strategy.ID] {
			return fmt.Errorf("invalid or duplicate transformation %q", strategy.ID)
		}
		seen[strategy.ID] = true
		fallbackFound = fallbackFound || strategy.ID == c.Fallback
	}
	if !fallbackFound {
		return fmt.Errorf("fallback %q is not a declared transformation", c.Fallback)
	}
	if err := validateIR(c.BaseIR); err != nil {
		return fmt.Errorf("base IR: %w", err)
	}
	for _, strategy := range c.Transformations {
		if err := validateIR(apply(c.BaseIR, strategy)); err != nil {
			return fmt.Errorf("transformation %q: %w", strategy.ID, err)
		}
	}
	if len(c.Required.Entities)+len(c.Required.Activities) == 0 {
		return errors.New("case must declare at least one structural requirement")
	}
	if err := validateIR(IR{Entities: c.Required.Entities, Activities: c.Required.Activities}); err != nil {
		return fmt.Errorf("required contract: %w", err)
	}
	return nil
}

func validateIR(ir IR) error {
	entities := make(map[string]bool, len(ir.Entities))
	for _, name := range ir.Entities {
		if !identifierPattern.MatchString(name) || entities[name] {
			return fmt.Errorf("invalid or duplicate entity %q", name)
		}
		entities[name] = true
	}
	activities := make(map[string]bool, len(ir.Activities))
	for _, activity := range ir.Activities {
		if !identifierPattern.MatchString(activity.Name) || activities[activity.Name] {
			return fmt.Errorf("invalid or duplicate activity %q", activity.Name)
		}
		activities[activity.Name] = true
		if len(activity.Outputs) != 1 {
			return fmt.Errorf("activity %s must have exactly one output in this Gooo syntax profile", activity.Name)
		}
		for _, name := range append(append([]string(nil), activity.Inputs...), activity.Outputs...) {
			if !entities[name] {
				return fmt.Errorf("activity %s references undeclared entity %q", activity.Name, name)
			}
		}
	}
	return nil
}

func apply(base IR, strategy Transformation) IR {
	out := IR{
		Entities:   append([]string(nil), base.Entities...),
		Activities: append([]Activity(nil), base.Activities...),
	}
	out.Entities = append(out.Entities, strategy.AddEntities...)
	out.Activities = append(out.Activities, strategy.AddActivities...)
	return out
}

func makeDecisionRequest(c Case) decisionRequest {
	options := make([]decisionOption, 0, len(c.Transformations))
	for _, strategy := range c.Transformations {
		options = append(options, decisionOption{ID: strategy.ID, Description: strategy.Description})
	}
	return decisionRequest{
		Schema: "gooo/typed-decision-request/v1", State: c.Task,
		Question: decisionQuestion{
			ID:           "ir_transformation",
			Instructions: "현재 과업에 필요한 IR 구조를 가장 잘 표현할 결정론적 변환 패턴을 고른다.",
			Options:      options,
		},
		Fallback: c.Fallback,
	}
}

func decide(goooBin string, request decisionRequest) (DecisionReceipt, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return DecisionReceipt{}, fmt.Errorf("encode decision request: %w", err)
	}
	file, err := os.CreateTemp("", "gooo-lab-decision-*.json")
	if err != nil {
		return DecisionReceipt{}, fmt.Errorf("create decision request: %w", err)
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return DecisionReceipt{}, fmt.Errorf("write decision request: %w", err)
	}
	if err := file.Close(); err != nil {
		return DecisionReceipt{}, fmt.Errorf("close decision request: %w", err)
	}
	cmd := exec.Command(goooBin, "decide", "--json", file.Name())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return DecisionReceipt{}, fmt.Errorf("gooo decide: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var receipt DecisionReceipt
	if err := json.Unmarshal(output, &receipt); err != nil {
		return DecisionReceipt{}, fmt.Errorf("decode decision receipt: %w", err)
	}
	return receipt, nil
}

func generateCandidate(goooBin string, c Case, id string, ir IR, root string) CandidateResult {
	result := CandidateResult{ID: id}
	if err := validateIR(ir); err != nil {
		result.Error = err.Error()
		return result
	}
	result.IRValid = true
	irBytes, err := json.Marshal(ir)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.IRSHA256 = digest(irBytes)
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		result.Error = fmt.Sprintf("create candidate directory: %v", err)
		return result
	}
	sourcePath := filepath.Join(dir, "candidate.gooo")
	source, err := renderSource(c.Package, ir)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		result.Error = fmt.Sprintf("write candidate source: %v", err)
		return result
	}
	generatedDir := filepath.Join(dir, "generated")
	start := time.Now()
	_, stderr, err := commandOutput("", goooBin, "generate", sourcePath, "--out", generatedDir)
	result.GenerationMS = millisecondsSince(start)
	if err != nil {
		result.Error = fmt.Sprintf("gooo generate: %v: %s", err, stderr)
		return result
	}
	goSourcePath := filepath.Join(generatedDir, "semantic.gooo.go")
	goSource, err := os.ReadFile(goSourcePath)
	if err != nil {
		result.Error = fmt.Sprintf("read generated Go: %v", err)
		return result
	}
	result.GeneratedSHA256 = digest(goSource)
	result.GeneratedBytes = int64(len(goSource))
	replayDir := filepath.Join(dir, "replay")
	start = time.Now()
	_, stderr, err = commandOutput("", goooBin, "generate", sourcePath, "--out", replayDir)
	result.ReplayMS = millisecondsSince(start)
	if err != nil {
		result.Error = fmt.Sprintf("gooo replay: %v: %s", err, stderr)
		return result
	}
	replaySource, err := os.ReadFile(filepath.Join(replayDir, "semantic.gooo.go"))
	if err != nil {
		result.Error = fmt.Sprintf("read replayed Go: %v", err)
		return result
	}
	result.ReplaySHA256 = digest(replaySource)
	result.ReplayCount = 2
	result.Deterministic = result.GeneratedSHA256 == result.ReplaySHA256
	if !result.Deterministic {
		result.Error = "generated Go changed on deterministic replay"
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, goSourcePath, goSource, parser.AllErrors)
	if err != nil {
		result.Error = fmt.Sprintf("parse generated Go: %v", err)
		return result
	}
	result.Coverage = scoreCoverage(c.Required, inspectGo(file))
	mod := []byte("module example.org/gooo-lab/" + c.ID + "/" + id + "\n\ngo 1.27.0\n")
	if err := os.WriteFile(filepath.Join(generatedDir, "go.mod"), mod, 0o644); err != nil {
		result.Error = fmt.Sprintf("write candidate go.mod: %v", err)
		return result
	}
	start = time.Now()
	_, stderr, err = commandOutput(generatedDir, "go", "build", ".")
	result.CompileMS = millisecondsSince(start)
	if err != nil {
		result.Error = fmt.Sprintf("go build: %v: %s", err, stderr)
		return result
	}
	result.Compiled = true
	return result
}

func commandOutput(dir, name string, args ...string) ([]byte, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

func renderSource(packageName string, ir IR) ([]byte, error) {
	if err := validateIR(ir); err != nil {
		return nil, err
	}
	var source strings.Builder
	fmt.Fprintf(&source, "package %s\nnamespace %s\n\n", packageName, packageName)
	for _, name := range ir.Entities {
		fmt.Fprintf(&source, "entity %s id %q\n", name, packageName+"://entity/"+kebab(name))
	}
	for _, activity := range ir.Activities {
		fmt.Fprintf(&source, "\nactivity %s(%s) -> %s\n", activity.Name, strings.Join(activity.Inputs, ", "), activity.Outputs[0])
	}
	return []byte(source.String()), nil
}

func inspectGo(file *ast.File) generatedShape {
	shape := generatedShape{entities: make(map[string]bool), activities: make(map[string]activityShape)}
	for _, declaration := range file.Decls {
		switch item := declaration.(type) {
		case *ast.GenDecl:
			for _, spec := range item.Specs {
				if typeSpec, ok := spec.(*ast.TypeSpec); ok {
					shape.entities[typeSpec.Name.Name] = true
				}
			}
		case *ast.FuncDecl:
			if item.Recv != nil || item.Type == nil {
				continue
			}
			activity := activityShape{}
			if item.Type.Params != nil {
				activity.inputs = fieldTypes(item.Type.Params)
			}
			if item.Type.Results != nil {
				activity.outputs = fieldTypes(item.Type.Results)
			}
			shape.activities[item.Name.Name] = activity
		}
	}
	return shape
}

type activityShape struct {
	inputs  []string
	outputs []string
}

type generatedShape struct {
	entities   map[string]bool
	activities map[string]activityShape
}

func fieldTypes(fields *ast.FieldList) []string {
	var types []string
	for _, field := range fields.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		name := expressionTypeName(field.Type)
		for range count {
			types = append(types, name)
		}
	}
	return types
}

func expressionTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return expressionTypeName(value.X)
	case *ast.SelectorExpr:
		return expressionTypeName(value.Sel)
	default:
		return ""
	}
}

func scoreCoverage(requirements Requirements, shape generatedShape) Coverage {
	var missing []string
	satisfied, total := 0, 0
	for _, entity := range requirements.Entities {
		total++
		if shape.entities[entity] {
			satisfied++
		} else {
			missing = append(missing, "entity:"+entity)
		}
	}
	for _, required := range requirements.Activities {
		total++
		actual, exists := shape.activities[required.Name]
		if exists {
			satisfied++
		} else {
			missing = append(missing, "activity:"+required.Name)
		}
		for _, input := range required.Inputs {
			total++
			if exists && takeType(actual.inputs, input) {
				satisfied++
			} else {
				missing = append(missing, "port:"+required.Name+":input:"+input)
			}
		}
		for _, output := range required.Outputs {
			total++
			if exists && takeType(actual.outputs, output) {
				satisfied++
			} else {
				missing = append(missing, "port:"+required.Name+":output:"+output)
			}
		}
	}
	coverage := Coverage{Satisfied: satisfied, Total: total, Missing: missing}
	if total > 0 {
		coverage.Ratio = float64(satisfied) / float64(total)
	}
	return coverage
}

func takeType(types []string, name string) bool {
	for index, value := range types {
		if value == name {
			types[index] = ""
			return true
		}
	}
	return false
}

func resultByID(results []CandidateResult, id string) (CandidateResult, bool) {
	for _, result := range results {
		if result.ID == id {
			return result, true
		}
	}
	return CandidateResult{}, false
}

func oracleCandidate(results []CandidateResult) (CandidateResult, bool) {
	ordered := append([]CandidateResult(nil), results...)
	ordered = ordered[:0]
	for _, result := range results {
		if result.Compiled && result.Deterministic {
			ordered = append(ordered, result)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Coverage.Ratio > ordered[j].Coverage.Ratio
	})
	if len(ordered) == 0 {
		return CandidateResult{}, false
	}
	return ordered[0], true
}

func kebab(name string) string {
	var out strings.Builder
	for index, r := range name {
		if r >= 'A' && r <= 'Z' {
			if index > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r + ('a' - 'A'))
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func millisecondsSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

var _ io.Writer = (*bytes.Buffer)(nil)
