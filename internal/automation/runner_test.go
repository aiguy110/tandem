package automation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
}

func TestRunnerRunsTypeScriptWithImportsArgsAndReports(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/lib.ts", `export const answer: number = 42;`)
	path := repo + "/.tandem/scripts/main.ts"
	source := `/** @tandem
name: runner test
wake:
  agentProfile: default-profile
*/
import { report } from "tandem:runtime";
import { answer } from "./lib.ts";
const typed: string = "works";
console.log(typed, answer, process.argv[2]);
report({wakeAgent: false});
report({wakeAgent: true, agentProfile: "override-profile", context: {answer}});
`
	if err := writeTestFile(path, source); err != nil {
		t.Fatal(err)
	}
	result, err := (Runner{}).Run(context.Background(), repo, ".tandem/scripts/main.ts", []string{"argument"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "works 42 argument\n" || result.Stderr != "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Report == nil || !result.Report.WakeAgent || result.Report.AgentProfile != "override-profile" {
		t.Fatalf("unexpected last report: %+v", result.Report)
	}
	var contextValue map[string]int
	if err := json.Unmarshal(result.Report.Context, &contextValue); err != nil || contextValue["answer"] != 42 {
		t.Fatalf("unexpected context %s: %v", result.Report.Context, err)
	}
	if result.Duration <= 0 {
		t.Fatalf("unexpected duration %v", result.Duration)
	}
}

func TestRunnerReturnsNonZeroWithoutInfrastructureError(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/fail.ts", `process.stderr.write("bad news\n"); process.exit(7);`)
	result, err := (Runner{}).Run(context.Background(), repo, ".tandem/scripts/fail.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || result.Stderr != "bad news\n" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunnerEvaluateHasHostAccess(t *testing.T) {
	requireNode(t)
	repo := t.TempDir()
	result, err := (Runner{}).Evaluate(context.Background(), repo, []byte(`
import { writeFileSync, readFileSync } from "node:fs";
writeFileSync("host-access.txt", "yes");
console.log(readFileSync("host-access.txt", "utf8"));
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "yes\n" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunnerBridgesGrantedTools(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/tool.ts", `
import { tools } from "tandem:runtime";
const value = await tools.example.echo({message: "hello"});
console.log(value.reply);
`)
	var calledName string
	runner := Runner{ToolHandler: func(_ context.Context, name string, arguments json.RawMessage) (any, error) {
		calledName = name
		var input struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(arguments, &input); err != nil {
			return nil, err
		}
		return map[string]string{"reply": input.Message + " world"}, nil
	}}
	result, err := runner.Run(context.Background(), repo, ".tandem/scripts/tool.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "hello world\n" || calledName != "example.echo" {
		t.Fatalf("unexpected result=%+v called=%q", result, calledName)
	}
}

func TestRunnerToolErrorsRejectPromise(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/tool.ts", `
import { tools } from "tandem:runtime";
await tools.call("example.denied");
`)
	runner := Runner{ToolHandler: func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("tool is not granted")
	}}
	result, err := runner.Run(context.Background(), repo, ".tandem/scripts/tool.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "tool is not granted") {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRunnerRejectsInvalidReportAtRuntime(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/bad.ts", `import { report } from "tandem:runtime"; report({wakeAgent: true, agentProfile: ""});`)
	result, err := (Runner{}).Run(context.Background(), repo, ".tandem/scripts/bad.ts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || !strings.Contains(result.Stderr, "agentProfile must be a non-empty string") {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestReportEffectiveAgentProfile(t *testing.T) {
	manifest := Manifest{Wake: &WakeSpec{AgentProfile: "frontmatter-default"}}
	tests := []struct {
		report Report
		want   string
	}{
		{Report{WakeAgent: false, AgentProfile: "ignored"}, ""},
		{Report{WakeAgent: true}, "frontmatter-default"},
		{Report{WakeAgent: true, AgentProfile: "per-call-override"}, "per-call-override"},
	}
	for _, tt := range tests {
		if got := tt.report.EffectiveAgentProfile(manifest); got != tt.want {
			t.Errorf("EffectiveAgentProfile(%+v) = %q, want %q", tt.report, got, tt.want)
		}
	}
}

func TestRunnerCancellation(t *testing.T) {
	requireNode(t)
	repo := makeRepoScript(t, ".tandem/scripts/wait.ts", `await new Promise(resolve => setTimeout(resolve, 60_000));`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := (Runner{}).Run(ctx, repo, ".tandem/scripts/wait.ts", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want deadline exceeded", err)
	}
}

func TestRunnerConfigurableCommand(t *testing.T) {
	repo := makeRepoScript(t, ".tandem/scripts/test.ts", ``)
	_, err := (Runner{NodeCommand: "definitely-not-a-real-tandem-node"}).Run(context.Background(), repo, ".tandem/scripts/test.ts", nil)
	if err == nil || !strings.Contains(err.Error(), "start automation script") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func writeTestFile(path, source string) error {
	return os.WriteFile(path, []byte(source), 0o644)
}
