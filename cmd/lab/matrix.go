package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type MatrixPlan struct {
	Schema   string   `json:"schema"`
	Domain   []int    `json:"domain"`
	Fallback string   `json:"fallback"`
	Intents  []Intent `json:"intents"`
	Routes   []Route  `json:"routes"`
}

type Intent struct {
	ID           string   `json:"id"`
	Task         string   `json:"task"`
	Default      int      `json:"default"`
	Rules        []Rule   `json:"rules"`
	Requirements []string `json:"requirements"`
}

type Rule struct {
	When  []Atom `json:"when"`
	Value int    `json:"value"`
	Input bool   `json:"input_value,omitempty"`
}

type Atom struct {
	Op    string `json:"op"`
	Value int    `json:"value,omitempty"`
	Upper int    `json:"upper,omitempty"`
	Mod   int    `json:"mod,omitempty"`
}

type Route struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Style       string   `json:"style"`
	Constructs  []string `json:"constructs"`
}

type MatrixDecision struct {
	IntentID     string          `json:"intent_id"`
	Receipt      DecisionReceipt `json:"receipt"`
	Top1Route    string          `json:"top1_route"`
	SampledRoute string          `json:"sampled_route"`
	SampleSeed   string          `json:"sample_seed"`
	LatencyMS    float64         `json:"latency_ms"`
}

type MatrixCandidate struct {
	ExperimentID       string   `json:"experiment_id"`
	IntentID           string   `json:"intent_id"`
	RouteID            string   `json:"route_id"`
	Requirements       []string `json:"requirements"`
	Covered            int      `json:"covered"`
	Total              int      `json:"total"`
	Completeness       float64  `json:"behavioral_completeness"`
	ClausesCovered     int      `json:"clauses_covered"`
	ClauseTotal        int      `json:"clauses_total"`
	ClauseCompleteness float64  `json:"rule_clause_completeness"`
	ExtraConstructs    []string `json:"extra_constructs,omitempty"`
	ConstructCoverage  float64  `json:"construct_coverage"`
	ConstructPrecision float64  `json:"construct_precision"`
	GeneratedBytes     int64    `json:"generated_bytes"`
	SourceSHA256       string   `json:"source_sha256"`
	ReplaySHA256       string   `json:"replay_sha256"`
	Deterministic      bool     `json:"deterministic"`
	Compiled           bool     `json:"compiled"`
	Error              string   `json:"error,omitempty"`
}

type MatrixReport struct {
	Schema                   string            `json:"schema"`
	PlanSHA256               string            `json:"plan_sha256"`
	GoooBaseSHA256           string            `json:"gooo_base_sha256"`
	GeneratedSHA256          string            `json:"generated_sha256"`
	GeneratedBytes           int64             `json:"generated_bytes"`
	ReplaySHA256             string            `json:"replay_sha256"`
	Deterministic            bool              `json:"deterministic"`
	ExperimentCount          int               `json:"experiment_count"`
	IntentCount              int               `json:"intent_count"`
	RouteCount               int               `json:"route_count"`
	DomainCount              int               `json:"domain_count"`
	BuildMS                  float64           `json:"build_ms"`
	ExecutionMS              float64           `json:"execution_ms"`
	DecisionLatencyMeanMS    float64           `json:"decision_latency_mean_ms"`
	DecisionLatencyP95MS     float64           `json:"decision_latency_p95_ms"`
	Decisions                []MatrixDecision  `json:"decisions"`
	Candidates               []MatrixCandidate `json:"candidates"`
	Top1MeanCompleteness     float64           `json:"top1_mean_completeness"`
	SampledMeanCompleteness  float64           `json:"sampled_mean_completeness"`
	OracleMeanCompleteness   float64           `json:"oracle_mean_completeness"`
	BaselineMeanCompleteness float64           `json:"baseline_mean_completeness"`
}

type executableResults map[string][]int

func runMatrixCommand(args []string) error {
	flags := flag.NewFlagSet("matrix", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	planPath := flags.String("plan", "", "matrix plan JSON path")
	outDir := flags.String("out", "", "output directory for source and report")
	goooBin := flags.String("gooo", os.Getenv("GOOO_BIN"), "Gooo CLI path; defaults to GOOO_BIN or gooo")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *planPath == "" || *outDir == "" {
		return errors.New("--plan and --out are required")
	}
	if *goooBin == "" {
		*goooBin = "gooo"
	}
	report, err := runMatrix(*planPath, *outDir, *goooBin)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(*outDir, "matrix-report.json"), data, 0o644); err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func runMatrix(planPath, outDir, goooBin string) (MatrixReport, error) {
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		return MatrixReport{}, fmt.Errorf("read plan: %w", err)
	}
	var plan MatrixPlan
	decoder := json.NewDecoder(bytes.NewReader(planBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return MatrixReport{}, fmt.Errorf("decode plan: %w", err)
	}
	if err := validateMatrixPlan(plan); err != nil {
		return MatrixReport{}, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return MatrixReport{}, err
	}
	generatedDir := filepath.Join(outDir, "generated")
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		return MatrixReport{}, err
	}
	baseSource := renderMatrixGooo(plan)
	basePath := filepath.Join(outDir, "intent-structure.gooo")
	if err := os.WriteFile(basePath, baseSource, 0o644); err != nil {
		return MatrixReport{}, err
	}
	if _, stderr, err := commandOutput("", goooBin, "generate", basePath, "--out", generatedDir); err != nil {
		return MatrixReport{}, fmt.Errorf("Gooo declaration generation failed: %v: %s", err, stderr)
	}
	goooBase, err := os.ReadFile(filepath.Join(generatedDir, "semantic.gooo.go"))
	if err != nil {
		return MatrixReport{}, fmt.Errorf("read Gooo projection: %w", err)
	}
	decisions := make([]MatrixDecision, 0, len(plan.Intents))
	decisionByIntent := make(map[string]MatrixDecision, len(plan.Intents))
	for _, intent := range plan.Intents {
		request := matrixDecisionRequest(plan, intent)
		decisionStart := time.Now()
		receipt, err := decide(goooBin, request)
		decisionLatency := millisecondsSince(decisionStart)
		if err != nil {
			return MatrixReport{}, err
		}
		top1 := receipt.Selected
		if !routeExists(plan.Routes, top1) {
			return MatrixReport{}, fmt.Errorf("intent %s selected undeclared route %q", intent.ID, top1)
		}
		seed := digest([]byte(intent.ID + "\x00" + receipt.RequestSHA256 + "\x00" + receipt.ModelRevision))
		sampled := weightedRoute(receipt, plan.Routes, seed)
		if sampled == "" {
			sampled = plan.Fallback
		}
		decision := MatrixDecision{IntentID: intent.ID, Receipt: receipt, Top1Route: top1, SampledRoute: sampled, SampleSeed: seed, LatencyMS: decisionLatency}
		decisions = append(decisions, decision)
		decisionByIntent[intent.ID] = decision
	}

	var program strings.Builder
	program.WriteString("package main\n\nimport (\n\t\"encoding/json\"\n\t\"os\"\n)\n\n")
	candidateSources := make(map[string][]byte, len(plan.Intents)*len(plan.Routes))
	for _, intent := range plan.Intents {
		for _, route := range plan.Routes {
			candidate := []byte(renderCandidateFunction(intent, route, plan.Domain))
			candidateSources[intent.ID+"__"+route.ID] = candidate
			program.Write(candidate)
			program.WriteByte('\n')
		}
	}
	program.WriteString(renderMatrixMain(plan))
	programBytes := []byte(program.String())
	if _, err := parser.ParseFile(token.NewFileSet(), "matrix_generated.go", programBytes, parser.AllErrors); err != nil {
		return MatrixReport{}, fmt.Errorf("generated matrix source parse: %w", err)
	}
	firstSource := append([]byte(nil), programBytes...)
	secondSource := []byte(program.String())
	firstHash, secondHash := digest(firstSource), digest(secondSource)
	if err := os.WriteFile(filepath.Join(generatedDir, "matrix_generated.go"), programBytes, 0o644); err != nil {
		return MatrixReport{}, err
	}
	if err := os.WriteFile(filepath.Join(generatedDir, "semantic.gooo.go"), goooBase, 0o644); err != nil {
		return MatrixReport{}, err
	}
	module := []byte("module example.org/gooo/metaprogramming-matrix\n\ngo 1.27.0\n")
	if err := os.WriteFile(filepath.Join(generatedDir, "go.mod"), module, 0o644); err != nil {
		return MatrixReport{}, err
	}
	binaryPath := filepath.Join(generatedDir, "matrix-runner")
	start := time.Now()
	_, stderr, err := commandOutput(generatedDir, "go", "build", "-o", binaryPath, ".")
	buildMS := millisecondsSince(start)
	if err != nil {
		return MatrixReport{}, fmt.Errorf("compile matrix candidates: %v: %s", err, stderr)
	}
	start = time.Now()
	stdout, stderr, err := commandOutput(generatedDir, binaryPath)
	executionMS := millisecondsSince(start)
	if err != nil {
		return MatrixReport{}, fmt.Errorf("execute matrix candidates: %v: %s", err, stderr)
	}
	var actual executableResults
	if err := json.Unmarshal(stdout, &actual); err != nil {
		return MatrixReport{}, fmt.Errorf("decode candidate results: %w", err)
	}
	actualSourceHash := digest(programBytes)
	candidates := make([]MatrixCandidate, 0, len(plan.Intents)*len(plan.Routes))
	byID := make(map[string]MatrixCandidate, len(plan.Intents)*len(plan.Routes))
	for _, intent := range plan.Intents {
		for _, route := range plan.Routes {
			id := intent.ID + "__" + route.ID
			got, exists := actual[id]
			if !exists || len(got) != len(plan.Domain) {
				return MatrixReport{}, fmt.Errorf("candidate %s returned %d outcomes, want %d", id, len(got), len(plan.Domain))
			}
			want := make([]int, len(plan.Domain))
			covered := 0
			for i, input := range plan.Domain {
				want[i] = evaluateIntent(intent, input)
				if got[i] == want[i] {
					covered++
				}
			}
			clausesCovered, clauseTotal := scoreRuleClauses(intent, plan.Domain, got)
			requiredConstructs := append([]string(nil), route.Constructs...)
			if (route.Style == "assign_ladder" || route.Style == "partial_assign") && len(intent.Rules) < 2 {
				requiredConstructs = removeString(requiredConstructs, "else")
			}
			structural, precision, extras := sourceConstructMetrics(programBytes, functionName(intent, route), requiredConstructs)
			candidateSource := candidateSources[id]
			replaySource := []byte(renderCandidateFunction(intent, route, plan.Domain))
			row := MatrixCandidate{
				ExperimentID: id, IntentID: intent.ID, RouteID: route.ID,
				Requirements: append([]string(nil), intent.Requirements...),
				Covered:      covered, Total: len(plan.Domain), Completeness: float64(covered) / float64(len(plan.Domain)),
				ClausesCovered: clausesCovered, ClauseTotal: clauseTotal,
				ClauseCompleteness: float64(clausesCovered) / float64(clauseTotal),
				ExtraConstructs:    extras, ConstructCoverage: structural, ConstructPrecision: precision,
				GeneratedBytes: int64(len(candidateSource)),
				SourceSHA256:   digest(candidateSource), ReplaySHA256: digest(replaySource),
				Deterministic: digest(candidateSource) == digest(replaySource), Compiled: true,
			}
			candidates = append(candidates, row)
			byID[id] = row
		}
	}

	top1Sum, sampleSum, oracleSum, baselineSum := 0.0, 0.0, 0.0, 0.0
	for _, intent := range plan.Intents {
		decision := decisionByIntent[intent.ID]
		top1Sum += byID[intent.ID+"__"+decision.Top1Route].Completeness
		sampleSum += byID[intent.ID+"__"+decision.SampledRoute].Completeness
		best := 0.0
		for _, route := range plan.Routes {
			if score := byID[intent.ID+"__"+route.ID].Completeness; score > best {
				best = score
			}
		}
		oracleSum += best
		baselineSum += byID[intent.ID+"__"+plan.Fallback].Completeness
	}
	denom := float64(len(plan.Intents))
	decisionLatencies := make([]float64, 0, len(decisions))
	for _, decision := range decisions {
		decisionLatencies = append(decisionLatencies, decision.LatencyMS)
	}
	sort.Float64s(decisionLatencies)
	p95Index := int(math.Ceil(float64(len(decisionLatencies))*0.95)) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	latencySum := 0.0
	for _, latency := range decisionLatencies {
		latencySum += latency
	}
	return MatrixReport{
		Schema: "gooo/metaprogramming-matrix-report/v1", PlanSHA256: digest(planBytes),
		GoooBaseSHA256: digest(goooBase), GeneratedSHA256: actualSourceHash, GeneratedBytes: int64(len(programBytes)),
		ReplaySHA256: secondHash, Deterministic: firstHash == secondHash,
		ExperimentCount: len(candidates), IntentCount: len(plan.Intents),
		RouteCount: len(plan.Routes), DomainCount: len(plan.Domain), BuildMS: buildMS, ExecutionMS: executionMS,
		DecisionLatencyMeanMS: latencySum / float64(len(decisionLatencies)), DecisionLatencyP95MS: decisionLatencies[p95Index],
		Decisions: decisions, Candidates: candidates,
		Top1MeanCompleteness: top1Sum / denom, SampledMeanCompleteness: sampleSum / denom,
		OracleMeanCompleteness: oracleSum / denom, BaselineMeanCompleteness: baselineSum / denom,
	}, nil
}

func validateMatrixPlan(plan MatrixPlan) error {
	if plan.Schema != "gooo/metaprogramming-matrix-plan/v1" || len(plan.Intents) != 10 || len(plan.Routes) != 10 || len(plan.Domain) < 3 {
		return errors.New("matrix plan must declare schema v1, exactly 10 intents, 10 routes, and at least 3 domain points")
	}
	if !routeExists(plan.Routes, plan.Fallback) {
		return fmt.Errorf("fallback route %q is undeclared", plan.Fallback)
	}
	seen := map[string]bool{}
	for _, input := range plan.Domain {
		if seen[fmt.Sprint(input)] {
			return fmt.Errorf("duplicate domain input %d", input)
		}
		seen[fmt.Sprint(input)] = true
	}
	seen = map[string]bool{}
	for _, route := range plan.Routes {
		if !strategyPattern.MatchString(route.ID) || route.Description == "" || seen[route.ID] || !validRouteStyle(route.Style) || len(route.Constructs) == 0 {
			return fmt.Errorf("invalid route %q", route.ID)
		}
		seen[route.ID] = true
	}
	seen = map[string]bool{}
	for _, intent := range plan.Intents {
		if !strategyPattern.MatchString(intent.ID) || intent.Task == "" || seen[intent.ID] || len(intent.Rules) == 0 {
			return fmt.Errorf("invalid intent %q", intent.ID)
		}
		seen[intent.ID] = true
		for _, rule := range intent.Rules {
			if len(rule.When) == 0 {
				return fmt.Errorf("intent %s has an empty rule", intent.ID)
			}
			for _, atom := range rule.When {
				if !validAtom(atom) {
					return fmt.Errorf("intent %s has invalid predicate %q", intent.ID, atom.Op)
				}
			}
		}
		witnessed := make([]bool, len(intent.Rules)+1)
		for _, input := range plan.Domain {
			matched := len(intent.Rules)
			for ruleIndex, rule := range intent.Rules {
				if matches(rule.When, input) {
					matched = ruleIndex
					break
				}
			}
			witnessed[matched] = true
		}
		for clause, hasWitness := range witnessed[:len(intent.Rules)] {
			if !hasWitness {
				return fmt.Errorf("intent %s has no domain witness for rule/default clause %d", intent.ID, clause)
			}
		}
	}
	return nil
}

func validRouteStyle(style string) bool {
	switch style {
	case "guard", "ladder", "switch", "assign_ladder", "helpers", "table", "closure", "assign_switch", "finite_map", "partial_assign":
		return true
	default:
		return false
	}
}

func validAtom(atom Atom) bool {
	switch atom.Op {
	case "eq", "ne", "lt", "le", "gt", "ge":
		return true
	case "between":
		return atom.Value <= atom.Upper
	case "mod_eq":
		return atom.Mod > 0 && atom.Value >= 0 && atom.Value < atom.Mod
	default:
		return false
	}
}

func renderMatrixGooo(plan MatrixPlan) []byte {
	var source strings.Builder
	source.WriteString("package main\nnamespace metaprogramming\n\n")
	source.WriteString("entity Intent id \"gooo://metaprogramming/intent\"\n")
	source.WriteString("entity CodegenReceipt id \"gooo://metaprogramming/codegen-receipt\"\n")
	for _, intent := range plan.Intents {
		name := strings.Title(intent.ID) // IDs are validated to lowercase ASCII tokens.
		fmt.Fprintf(&source, "activity Plan%s(Intent) -> CodegenReceipt\n", name)
	}
	source.WriteString("activity EvaluateCodegen(CodegenReceipt) -> CodegenReceipt\n")
	return []byte(source.String())
}

func matrixDecisionRequest(plan MatrixPlan, intent Intent) decisionRequest {
	options := make([]decisionOption, len(plan.Routes))
	for i, route := range plan.Routes {
		options[i] = decisionOption{ID: route.ID, Description: route.Description}
	}
	return decisionRequest{
		Schema: "gooo/typed-decision-request/v1", State: intent.Task,
		Question: decisionQuestion{ID: "codegen_route", Instructions: "명시된 의도에 맞는 결정론적 IR 조립 경로를 선택한다. 경로는 코드 텍스트를 직접 만들지 않고 검증 가능한 조립 규칙만 지정한다.", Options: options},
		Fallback: plan.Fallback,
	}
}

func weightedRoute(receipt DecisionReceipt, routes []Route, seed string) string {
	if len(receipt.Probabilities) == 0 {
		return ""
	}
	var sum float64
	for _, route := range routes {
		p := receipt.Probabilities[route.ID]
		if p > 0 && !math.IsNaN(p) && !math.IsInf(p, 0) {
			sum += p
		}
	}
	if sum <= 0 {
		return ""
	}
	seedBytes := sha256.Sum256([]byte(seed))
	unit := float64(binary.BigEndian.Uint64(seedBytes[:8])) / float64(math.MaxUint64)
	cutoff := unit * sum
	for _, route := range routes {
		cutoff -= receipt.Probabilities[route.ID]
		if cutoff < 0 {
			return route.ID
		}
	}
	return routes[len(routes)-1].ID
}

func routeExists(routes []Route, id string) bool {
	for _, route := range routes {
		if route.ID == id {
			return true
		}
	}
	return false
}

func removeString(values []string, target string) []string {
	filtered := values[:0]
	for _, value := range values {
		if value != target {
			filtered = append(filtered, value)
		}
	}
	return filtered
}

func evaluateIntent(intent Intent, input int) int {
	for _, rule := range intent.Rules {
		if matches(rule.When, input) {
			if rule.Input {
				return input
			}
			return rule.Value
		}
	}
	return intent.Default
}

func scoreRuleClauses(intent Intent, domain, actual []int) (int, int) {
	activeClauses := make([]bool, len(intent.Rules)+1)
	for _, input := range domain {
		matched := len(intent.Rules)
		for ruleIndex, rule := range intent.Rules {
			if matches(rule.When, input) {
				matched = ruleIndex
				break
			}
		}
		activeClauses[matched] = true
	}
	clauseTotal := 0
	for _, active := range activeClauses {
		if active {
			clauseTotal++
		}
	}
	clauseCovered := 0
	for clause, active := range activeClauses {
		if !active {
			continue
		}
		witnessed, complete := false, true
		for index, input := range domain {
			matched := -1
			for ruleIndex, rule := range intent.Rules {
				if matches(rule.When, input) {
					matched = ruleIndex
					break
				}
			}
			belongs := matched == clause || (clause == len(intent.Rules) && matched == -1)
			if belongs {
				witnessed = true
				if actual[index] != evaluateIntent(intent, input) {
					complete = false
				}
			}
		}
		if witnessed && complete {
			clauseCovered++
		}
	}
	return clauseCovered, clauseTotal
}

func matches(atoms []Atom, input int) bool {
	for _, atom := range atoms {
		ok := false
		switch atom.Op {
		case "eq":
			ok = input == atom.Value
		case "ne":
			ok = input != atom.Value
		case "lt":
			ok = input < atom.Value
		case "le":
			ok = input <= atom.Value
		case "gt":
			ok = input > atom.Value
		case "ge":
			ok = input >= atom.Value
		case "between":
			ok = input >= atom.Value && input <= atom.Upper
		case "mod_eq":
			ok = ((input%atom.Mod)+atom.Mod)%atom.Mod == atom.Value
		}
		if !ok {
			return false
		}
	}
	return true
}

func condition(rule Rule, input string) string {
	parts := make([]string, 0, len(rule.When))
	for _, atom := range rule.When {
		var expr string
		switch atom.Op {
		case "eq":
			expr = fmt.Sprintf("%s == %d", input, atom.Value)
		case "ne":
			expr = fmt.Sprintf("%s != %d", input, atom.Value)
		case "lt":
			expr = fmt.Sprintf("%s < %d", input, atom.Value)
		case "le":
			expr = fmt.Sprintf("%s <= %d", input, atom.Value)
		case "gt":
			expr = fmt.Sprintf("%s > %d", input, atom.Value)
		case "ge":
			expr = fmt.Sprintf("%s >= %d", input, atom.Value)
		case "between":
			expr = fmt.Sprintf("%s >= %d && %s <= %d", input, atom.Value, input, atom.Upper)
		case "mod_eq":
			expr = fmt.Sprintf("((%s %% %d) + %d) %% %d == %d", input, atom.Mod, atom.Mod, atom.Mod, atom.Value)
		}
		parts = append(parts, "("+expr+")")
	}
	return strings.Join(parts, " && ")
}

func resultExpression(rule Rule, input string) string {
	if rule.Input {
		return input
	}
	return fmt.Sprint(rule.Value)
}

func functionName(intent Intent, route Route) string {
	return "Codegen_" + strings.Title(intent.ID) + "_" + strings.Title(route.ID)
}

func renderCandidateFunction(intent Intent, route Route, domain []int) string {
	name := functionName(intent, route)
	var b strings.Builder
	fmt.Fprintf(&b, "func %s(input int) int {\n", name)
	rules := intent.Rules
	if route.Style == "partial_assign" && len(rules) > 2 {
		rules = rules[:2]
	}
	switch route.Style {
	case "guard":
		for _, rule := range rules {
			fmt.Fprintf(&b, "\tif %s { return %s }\n", condition(rule, "input"), resultExpression(rule, "input"))
		}
		fmt.Fprintf(&b, "\treturn %d\n", intent.Default)
	case "ladder":
		writeIfElse(&b, rules, intent.Default, "input", 1)
	case "switch":
		b.WriteString("\tswitch {\n")
		for _, rule := range rules {
			fmt.Fprintf(&b, "\tcase %s: return %s\n", condition(rule, "input"), resultExpression(rule, "input"))
		}
		fmt.Fprintf(&b, "\tdefault: return %d\n\t}\n", intent.Default)
	case "assign_ladder", "partial_assign":
		fmt.Fprintf(&b, "\tresult := %d\n", intent.Default)
		writeAssignLadder(&b, rules, "input")
		b.WriteString("\treturn result\n")
	case "helpers":
		for index, rule := range intent.Rules {
			fmt.Fprintf(&b, "\tif %s_%d(input) { return %s }\n", name, index, resultExpression(rule, "input"))
		}
		fmt.Fprintf(&b, "\treturn %d\n", intent.Default)
	case "table":
		fmt.Fprintf(&b, "\trules := []struct { when func(int) bool; value func(int) int }{\n")
		for _, rule := range intent.Rules {
			fmt.Fprintf(&b, "\t\t{func(v int) bool { return %s }, func(v int) int { return %s }},\n", condition(rule, "v"), resultExpression(rule, "v"))
		}
		b.WriteString("\t}\n\tfor _, rule := range rules { if rule.when(input) { return rule.value(input) } }\n")
		fmt.Fprintf(&b, "\treturn %d\n", intent.Default)
	case "closure":
		b.WriteString("\tchoose := func() int {\n")
		for _, rule := range intent.Rules {
			fmt.Fprintf(&b, "\t\tif %s { return %s }\n", condition(rule, "input"), resultExpression(rule, "input"))
		}
		fmt.Fprintf(&b, "\t\treturn %d\n\t}\n\treturn choose()\n", intent.Default)
	case "assign_switch":
		fmt.Fprintf(&b, "\tresult := %d\n\tswitch {\n", intent.Default)
		for _, rule := range intent.Rules {
			fmt.Fprintf(&b, "\tcase %s: result = %s\n", condition(rule, "input"), resultExpression(rule, "input"))
		}
		b.WriteString("\t}\n\treturn result\n")
	case "finite_map":
		b.WriteString("\tresults := map[int]int{\n")
		for _, input := range domain {
			fmt.Fprintf(&b, "\t\t%d: %d,\n", input, evaluateIntent(intent, input))
		}
		fmt.Fprintf(&b, "\t}\n\tif result, ok := results[input]; ok { return result }; return %d\n", intent.Default)
	}
	b.WriteString("}\n")
	if route.Style == "helpers" {
		for index, rule := range intent.Rules {
			fmt.Fprintf(&b, "func %s_%d(input int) bool { return %s }\n", name, index, condition(rule, "input"))
		}
	}
	return b.String()
}

func writeIfElse(b *strings.Builder, rules []Rule, fallback int, input string, indent int) {
	pad := strings.Repeat("\t", indent)
	if len(rules) == 0 {
		fmt.Fprintf(b, "%sreturn %d\n", pad, fallback)
		return
	}
	rule := rules[0]
	fmt.Fprintf(b, "%sif %s { return %s } else {\n", pad, condition(rule, input), resultExpression(rule, input))
	writeIfElse(b, rules[1:], fallback, input, indent+1)
	fmt.Fprintf(b, "%s}\n", pad)
}

func writeAssignLadder(b *strings.Builder, rules []Rule, input string) {
	if len(rules) == 0 {
		return
	}
	for index, rule := range rules {
		if index == 0 {
			fmt.Fprintf(b, "\tif %s { result = %s }", condition(rule, input), resultExpression(rule, input))
		} else {
			fmt.Fprintf(b, " else if %s { result = %s }", condition(rule, input), resultExpression(rule, input))
		}
	}
	b.WriteByte('\n')
}

func renderMatrixMain(plan MatrixPlan) string {
	var b strings.Builder
	b.WriteString("func main() {\n\tresults := map[string][]int{}\n")
	for _, intent := range plan.Intents {
		for _, route := range plan.Routes {
			name := functionName(intent, route)
			id := intent.ID + "__" + route.ID
			fmt.Fprintf(&b, "\tresults[%q] = []int{", id)
			for _, input := range plan.Domain {
				fmt.Fprintf(&b, "%s(%d),", name, input)
			}
			b.WriteString("}\n")
		}
	}
	b.WriteString("\t_ = json.NewEncoder(os.Stdout).Encode(results)\n}\n")
	return b.String()
}

func sourceConstructMetrics(source []byte, function string, required []string) (float64, float64, []string) {
	file, err := parser.ParseFile(token.NewFileSet(), "matrix_generated.go", source, 0)
	if err != nil {
		return 0, 0, nil
	}
	var functions []*ast.FuncDecl
	for _, declaration := range file.Decls {
		if candidate, ok := declaration.(*ast.FuncDecl); ok && (candidate.Name.Name == function || strings.HasPrefix(candidate.Name.Name, function+"_")) {
			functions = append(functions, candidate)
		}
	}
	if len(functions) == 0 {
		return 0, 0, nil
	}
	found := map[string]bool{}
	for _, fn := range functions {
		ast.Inspect(fn, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.IfStmt:
				found["if"] = true
				if value.Else != nil {
					found["else"] = true
				}
			case *ast.SwitchStmt, *ast.TypeSwitchStmt:
				found["switch"] = true
			case *ast.AssignStmt, *ast.DeclStmt:
				found["assignment"] = true
			case *ast.ReturnStmt:
				found["return"] = true
			case *ast.RangeStmt:
				found["range"] = true
			case *ast.FuncLit:
				found["closure"] = true
			case *ast.CallExpr:
				found["call"] = true
			case *ast.BinaryExpr:
				found["condition"] = true
			}
			return true
		})
	}
	if len(required) == 0 {
		return 1, 1, nil
	}
	covered := 0
	requiredSet := make(map[string]bool, len(required))
	for _, construct := range required {
		requiredSet[construct] = true
		if found[construct] {
			covered++
		}
	}
	var extras []string
	for construct := range found {
		if !requiredSet[construct] {
			extras = append(extras, construct)
		}
	}
	sort.Strings(extras)
	if len(found) == 0 {
		return float64(covered) / float64(len(required)), 0, extras
	}
	precision := float64(len(found)-len(extras)) / float64(len(found))
	return float64(covered) / float64(len(required)), precision, extras
}

func sortedRouteIDs(routes []Route) []string {
	ids := make([]string, len(routes))
	for i, route := range routes {
		ids[i] = route.ID
	}
	sort.Strings(ids)
	return ids
}
